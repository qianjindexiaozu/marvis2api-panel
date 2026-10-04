// chat 主链路：选择会话账号 → 上游转发 → SSE 透传/聚合 → 记账。
package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/kernel"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/logfmt"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/usage"
)

// chat POST /v1/chat/completions。
//
// 单次请求没有换号重试：账号未配置 / 冷却中 / 在途占满时返回明确状态码，
// 上游失败则分类记账后把上游原文透传给客户端（请求级错误不罚账号）。
func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	model := bodyModel(body)
	if h.cfg.Kernels != nil && kernel.Installed() {
		h.chatViaKernel(w, r, body, model)
		return
	}
	hint := r.Header.Get("X-Marvis-Account")
	a, pickErr := h.chooseAccount(r.Context(), hint, body, r.Header)
	if errors.Is(pickErr, errCooling) {
		h.writeUnavailable(w, model, "")
		return
	}
	if pickErr != nil || a == nil || a.TokenValue() == "" {
		msg := "尚未配置 Marvis 凭证：请在面板扫码添加微信或 QQ 账号"
		if pickErr != nil {
			msg = pickErr.Error()
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, msg, "no_account")
		return
	}
	label := a.Label()
	w.Header().Set("X-Marvis-Account", a.ID())
	if !h.cfg.State.Acquire() {
		writeOpenAIError(w, http.StatusServiceUnavailable,
			"在途请求已达上限（max_in_flight），请稍后重试", "in_flight_full")
		return
	}
	defer h.cfg.State.Release()

	started := time.Now()
	outBody := h.cfg.Upstream.PrepareBody(body)
	streamReq := upstream.IsStreamBody(outBody) // 出站 stream 标志（上游响应 CT 恒为 text/plain，不可依此判流式）
	res, err := h.cfg.Upstream.Chat(r.Context(), a, outBody)
	headerAt := time.Since(started) // 上游响应头到达时刻（TTFB 观测点）
	if err != nil {
		// 客户端主动断开：不再记账（上游请求已被 ctx 取消）。
		if r.Context().Err() != nil {
			return
		}
		if ue, ok := upstream.AsUpstreamError(err); ok {
			switch ue.Kind {
			case upstream.ErrPromptTooLong, upstream.ErrContentBlocked:
				// 请求级错误：不罚账号，末端透传上游原文。
				h.writeUpstreamError(w, ue)
				h.logChat(model, label, ue.Status, headerAt, time.Since(started), -1, streamReq)
				return
			case upstream.ErrClient:
				// 其他 4xx：不罚账号（不冷却/不熔断/不计错），原文透传。
				h.writeUpstreamError(w, ue)
				h.logChat(model, label, ue.Status, headerAt, time.Since(started), -1, streamReq)
				return
			default:
				h.noteRouteFailure(a, ue.Kind)
				if h.isActive(a) {
					h.classifyAndNote(ue)
				}
				h.writeUpstreamErrorWithRetry(w, ue)
				h.logChat(model, label, ue.Status, headerAt, time.Since(started), -1, streamReq)
				return
			}
		}
		h.cfg.State.NoteError(state.ErrServer, 0, time.Time{}, "connect: "+err.Error())
		log.Printf("[chat] err=%v", err)
		writeOpenAIError(w, http.StatusBadGateway, "上游请求失败: "+err.Error(), "upstream_error")
		h.logChat(model, label, http.StatusBadGateway, headerAt, time.Since(started), -1, streamReq)
		return
	}
	// 上游成功响应（Chat 内已把 ≥400 分类为 *Error 返回）。
	h.serveChatSuccess(w, r, a, model, res, started, headerAt, streamReq)
}

// writeUnavailable 账号冷却/熔断中的拒绝响应：带上恢复倒计时（Retry-After）
// 与人读原因，让客户端与面板都能看出「什么时候能再来」。
func (h *Handler) writeUnavailable(w http.ResponseWriter, model, label string) {
	st := h.cfg.State.Status()
	msg := "账号暂不可用：" + st.ReasonText()
	if !st.Until.IsZero() {
		if d := time.Until(st.Until); d > 0 {
			w.Header().Set("Retry-After", retryAfter(d))
			msg += "（预计 " + st.Until.Format("15:04:05") + " 恢复）"
		}
	}
	log.Printf("[chat] rejected state=%s reason=%s", st.State, st.Reason)
	h.logChat(model, label, http.StatusServiceUnavailable, 0, 0, -1, false)
	writeOpenAIError(w, http.StatusServiceUnavailable, msg, "account_unavailable")
}

// retryAfter 时长的整秒字符串（Retry-After 头；不足 1 秒按 1 秒）。
func retryAfter(d time.Duration) string {
	n := int(d.Seconds() + 0.5)
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}

// writeUpstreamError 把上游错误按 OpenAI 错误形态透传给客户端（保留上游原文 message）。
func (h *Handler) writeUpstreamError(w http.ResponseWriter, ue *upstream.Error) {
	status := ue.Status
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	writeOpenAIError(w, status, ue.Msg, ue.Kind.String())
}

// writeUpstreamErrorWithRetry 已记账的失败：透传上游原文，并在冷却期已知时
// 带上 Retry-After（限流 / 配额耗尽 / 认证失效都产生了冷却）。
func (h *Handler) writeUpstreamErrorWithRetry(w http.ResponseWriter, ue *upstream.Error) {
	status := ue.Status
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if st := h.cfg.State.Status(); !st.Until.IsZero() {
		if d := time.Until(st.Until); d > 0 {
			w.Header().Set("Retry-After", retryAfter(d))
		}
	}
	writeOpenAIError(w, status, ue.Msg, ue.Kind.String())
}

// serveChatSuccess 200 响应的分发：流式 SSE 透传 / 非流式聚合。
// 流式判定看出站请求的 stream 标志（真实上游响应 Content-Type 恒为
// text/plain，text/event-stream 判读不可用）。
func (h *Handler) serveChatSuccess(w http.ResponseWriter, r *http.Request, a *account.Auth, model string, res *upstream.ChatResult, started time.Time, headerAt time.Duration, streamReq bool) {
	defer res.Body.Close()
	h.cfg.State.NoteSuccess()

	if !streamReq {
		// 非流式：聚合为单响应（顺带解析 usage）。
		raw := readAllLimit(res.Body, 64<<20)
		u := parseUsageObject(raw)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		h.recordUsage(a, model, u, started, headerAt, false, http.StatusOK)
		return
	}

	// 流式：SSE 逐行透传 + 边读边解析 usage（上游最终 chunk / include_usage 尾块携带）。
	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if canFlush {
		flusher.Flush()
	}

	var u usageTotals
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64*1024), 8<<20) // 单行上限 8MB（大 tool_call 参数）
	for sc.Scan() {
		line := sc.Text()
		if ut, ok := scanUsage(line); ok {
			u = ut
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			break // 客户端断开：停止读取，记账已有数据
		}
		if canFlush {
			flusher.Flush()
		}
		// [DONE] 之后按容错继续读到 EOF。
	}
	h.recordUsage(a, model, u, started, headerAt, true, http.StatusOK)
}

// recordUsage 用量记账 + 逐请求日志行。
func (h *Handler) recordUsage(a *account.Auth, model string, u usageTotals, started time.Time, headerAt time.Duration, stream bool, status int) {
	if model == "" {
		model = "-"
	}
	total := time.Since(started)
	if h.cfg.Usage != nil {
		h.cfg.Usage.Record(usage.Record{
			Time: started, AccountID: a.Label(), Model: model,
			PromptTokens: u.prompt, CompletionTokens: u.completion,
			DurationMs: total.Milliseconds(), Status: status, Stream: stream,
		})
	}
	toks := -1 // usage 缺失 → tok=-
	if u.prompt > 0 || u.completion > 0 {
		toks = int(u.completion)
		h.noteSpend(a, u.prompt+u.completion)
	}
	h.logChat(model, a.Label(), status, headerAt, total, toks, stream)
	// /v1/stats 聚合埋点（单一记账点：成功路径统一经此）。
	h.metricsInst().Record(model, stream, status, total, u.prompt, u.completion)
}

// logChat 逐请求一行表格日志（成功与失败统一出口，日志格式权威在 logfmt）。
func (h *Handler) logChat(model, label string, status int, headerAt, total time.Duration, toks int, stream bool) {
	var seq int64
	if h.cfg.Seq != nil {
		seq = h.cfg.Seq()
	}
	log.Print(logfmt.ChatLine(seq, status, "", label, model, headerAt, total, toks, stream))
}

// usageTotals SSE / JSON 解析出的 token 用量。
type usageTotals struct {
	prompt     int64
	completion int64
}

// scanUsage 从一行 SSE（data: {...}）解析 usage 字段。无 usage 返回 false
// （中间 chunk 恒无 usage，只在最终 chunk / include_usage 尾块出现）。
func scanUsage(line string) (usageTotals, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "data:") {
		return usageTotals{}, false
	}
	payload := strings.TrimSpace(s[len("data:"):])
	if payload == "" || payload == "[DONE]" {
		return usageTotals{}, false
	}
	var obj struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil || obj.Usage == nil {
		return usageTotals{}, false
	}
	return usageTotals{prompt: obj.Usage.PromptTokens, completion: obj.Usage.CompletionTokens}, true
}

// parseUsageObject 从非流式响应 JSON 解析 usage。
func parseUsageObject(raw []byte) usageTotals {
	var obj struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Usage == nil {
		return usageTotals{}
	}
	return usageTotals{prompt: obj.Usage.PromptTokens, completion: obj.Usage.CompletionTokens}
}

// bodyModel 提取请求体 model 字段（日志/记账用；缺失返回 ""）。
func bodyModel(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	return obj.Model
}
