package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/kernel"
)

func (h *Handler) pickAccount(hint string) (*account.Auth, error) {
	if hint != "" && h.cfg.Accounts != nil {
		a, err := h.cfg.Accounts.Load(hint)
		if err != nil || a == nil {
			return nil, fmt.Errorf("没有这个账号")
		}
		return a, nil
	}
	if h.cfg.Accounts != nil {
		if id := h.cfg.Accounts.Active(); id != "" {
			if a, err := h.cfg.Accounts.Load(id); err == nil && a != nil {
				return a, nil
			}
		}
		if list, err := h.cfg.Accounts.List(); err == nil && len(list) == 1 {
			return list[0], nil
		}
	}
	if a := h.cfg.State.Auth(); a != nil && a.TokenValue() != "" {
		return a, nil
	}
	return nil, fmt.Errorf("尚未配置 Marvis 凭证：请在面板扫码添加微信或 QQ 账号")
}

// chatViaKernel 把请求交给该账号自己的官方内核，再转成 OpenAI 响应。
// 本机装了 Marvis 时走这条，不再让网关自己去加密。
func (h *Handler) chatViaKernel(w http.ResponseWriter, r *http.Request, body []byte, model string) {
	prompt, gotModel, stream, err := kernel.Prompt(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if model == "" {
		model = gotModel
	}
	a, err := h.chooseAccount(r.Context(), r.Header.Get("X-Marvis-Account"), body, r.Header)
	if errors.Is(err, errCooling) {
		h.writeUnavailable(w, model, "")
		return
	}
	if err != nil || a == nil {
		msg := "尚未配置 Marvis 凭证：请在面板扫码添加微信或 QQ 账号"
		if err != nil {
			msg = err.Error()
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, msg, "no_account")
		return
	}
	w.Header().Set("X-Marvis-Account", a.ID())
	in, err := h.cfg.Kernels.Pick(a.ID())
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, err.Error(), "no_account")
		return
	}
	if err := in.WaitReady(r.Context()); err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "账号内核还没准备好: "+err.Error(), "kernel_not_ready")
		return
	}
	if !h.cfg.State.Acquire() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "在途请求已达上限（max_in_flight），请稍后重试", "in_flight_full")
		return
	}
	defer h.cfg.State.Release()

	started := time.Now()
	a = in.Auth()
	label := a.Label()
	ch, err := in.Run(r.Context(), prompt)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error(), "kernel_error")
		h.logChat(model, label, http.StatusBadGateway, 0, time.Since(started), -1, stream)
		return
	}
	id := "chatcmpl-kernel"
	created := time.Now().Unix()
	var text string
	var u usageTotals
	headerAt := time.Duration(0)
	wrote := false
	flusher, canFlush := w.(http.Flusher)
	for d := range ch {
		if d.Err != nil {
			if !wrote {
				writeOpenAIError(w, http.StatusBadGateway, d.Err.Error(), "kernel_error")
				h.logChat(model, label, http.StatusBadGateway, headerAt, time.Since(started), -1, stream)
				return
			}
			writeSSE(w, map[string]any{"error": map[string]any{"message": d.Err.Error()}})
			return
		}
		if d.Prompt > 0 || d.Compl > 0 {
			u.prompt, u.completion = d.Prompt, d.Compl
		}
		if d.Text == "" && !d.Done {
			continue
		}
		if headerAt == 0 && d.Text != "" {
			headerAt = time.Since(started)
		}
		text += d.Text
		if !stream {
			if d.Done {
				break
			}
			continue
		}
		if !wrote {
			wrote = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
		}
		if d.Text != "" {
			delta := map[string]any{"content": d.Text}
			if text == d.Text {
				delta["role"] = "assistant"
			}
			writeSSE(w, map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
			})
			if canFlush {
				flusher.Flush()
			}
		}
		if d.Done {
			writeSSE(w, map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if canFlush {
				flusher.Flush()
			}
			h.recordUsage(a, model, u, started, headerAt, true, http.StatusOK)
			return
		}
	}
	if stream {
		if !wrote {
			writeOpenAIError(w, http.StatusBadGateway, "内核没有返回内容", "kernel_error")
			h.logChat(model, label, http.StatusBadGateway, headerAt, time.Since(started), -1, true)
			return
		}
		h.recordUsage(a, model, u, started, headerAt, true, http.StatusOK)
		return
	}
	if text == "" {
		writeOpenAIError(w, http.StatusBadGateway, "内核没有返回内容", "kernel_error")
		h.logChat(model, label, http.StatusBadGateway, headerAt, time.Since(started), -1, false)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(mustJSON(map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": u.prompt, "completion_tokens": u.completion, "total_tokens": u.prompt + u.completion},
	}))
	h.recordUsage(a, model, u, started, headerAt, false, http.StatusOK)
}

func writeSSE(w http.ResponseWriter, v any) {
	_, _ = io.WriteString(w, "data: "+string(mustJSON(v))+"\n\n")
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
