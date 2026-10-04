// Package gateway 提供 OpenAI 兼容路由：/v1/chat/completions、/v1/models、
// /healthz、/status。
//
// 本包负责鉴权、按请求与会话选择账号、上游转发、错误分类和用量记录。
// 单次请求不换号重试；没有可用账号时返回明确的状态码。
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/apikey"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/httpauth"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/kernel"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/livecfg"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/metrics"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/usage"
)

// Config handler 依赖（main 装配注入）。
type Config struct {
	State    *state.State
	Upstream *upstream.Client
	// Live 运行期可变密钥（面板热改 api_key）。
	Live *livecfg.Holder
	// APIKey Live 为 nil 时的静态回落。
	APIKey string
	// Usage 用量记录器；nil = 不记录。
	Usage *usage.Recorder
	// Metrics /v1/stats 聚合器；nil = 用进程级全局单例（测试可注入独立实例）。
	Metrics *metrics.Metrics
	// Version 网关版本（/status、/healthz 透出）。
	Version string

	// SoftCooldown 429 软冷却基数（来自 config，livecfg 热改）。
	SoftCooldown time.Duration
	// ModelsTimeout 拉模型列表的单请求超时。
	ModelsTimeout time.Duration
	// Seq 返回进程级请求序号（请求日志表格行用）；nil = 不编号。
	Seq func() int64
	// Kernels 每账号一份官方内核。本机装了 Marvis 时聊天走这里。
	Kernels *kernel.Pool
	// Accounts 多账号目录。容器里没有官方内核时，按这里选号走上游 HTTP。
	Accounts *account.Store
	// Keys 额外的 API Key。配置里的 api_key 仍然有效。
	Keys *apikey.Store
}

// Handler 网关 handler。
type Handler struct {
	cfg    Config
	mux    *http.ServeMux
	seq    int64
	mu     sync.Mutex
	routes *router
}

// New 构建 handler 并注册路由。
func New(cfg Config) *Handler {
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second
	}
	if cfg.ModelsTimeout <= 0 {
		cfg.ModelsTimeout = 15 * time.Second
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), routes: newRouter()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chat))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /v1/models/{id...}", h.withAuth(h.modelByID))
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("POST /v1/stats/reset", h.withAuth(h.statsReset))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	return h
}

// metricsInst 生效的聚合器（注入优先，回落进程级全局）。
func (h *Handler) metricsInst() *metrics.Metrics {
	if h.cfg.Metrics != nil {
		return h.cfg.Metrics
	}
	return metrics.Global()
}

// Metrics 暴露生效的统计聚合器（面板同进程代理复用同一份数据）。
func (h *Handler) Metrics() *metrics.Metrics { return h.metricsInst() }

// stats 处理 GET /v1/stats：按模型聚合的请求统计（社区面板数据源，
// 形态对齐 workbuddy2api）。
func (h *Handler) stats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.metricsInst().SnapshotOf())
}

// statsReset 处理 POST /v1/stats/reset：清空累计，便于观察增量。
func (h *Handler) statsReset(w http.ResponseWriter, _ *http.Request) {
	h.metricsInst().Reset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ServeHTTP 统一入口（路由分发）。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// nextSeq 进程级请求序号。
func (h *Handler) nextSeq() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	return h.seq
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (h *Handler) apiKey() string {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load().APIKey
	}
	return h.cfg.APIKey
}

// softCooldown 当前生效的软冷却基数（Live 优先）。
func (h *Handler) softCooldown() time.Duration {
	if h.cfg.Live != nil {
		if d := h.cfg.Live.Load().SoftCooldown; d > 0 {
			return d
		}
	}
	return h.cfg.SoftCooldown
}

// allowKey 有密钥库时只认库里启用的 key。没有密钥库时仍用配置里的 api_key，空则不鉴权。
func (h *Handler) allowKey(r *http.Request) bool {
	if h.cfg.Keys != nil {
		return h.cfg.Keys.Accept(bearerValue(r))
	}
	key := h.apiKey()
	if key == "" {
		return true
	}
	return httpauth.VerifyBearer(r, key)
}

func bearerValue(r *http.Request) string {
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, prefix) {
		return authz[len(prefix):]
	}
	return ""
}

// withAuth Bearer 鉴权（httpauth 常量时间比较）；api_key 为空 = 不鉴权放行。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.allowKey(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "无效的 API 密钥", "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// healthz 健康检查（免鉴权，宿主探活 / 负载均衡接入点）。
// healthy=0 时返回 503：无可用账号的网关对调用方是"坏上游"，探活应当报警。
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	_, healthy := h.cfg.State.Summary()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Service", "marvis2api")
	if healthy > 0 {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = w.Write([]byte(`{"healthy":` + strconv.Itoa(healthy) + `,"total":1,"service":"marvis2api"}`))
}

// status 账号状态（鉴权）：单账号快照 + 版本。
func (h *Handler) status(w http.ResponseWriter, _ *http.Request) {
	total, healthy := h.cfg.State.Summary()
	st := h.cfg.State.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"service":         "marvis2api",
		"version":         h.cfg.Version,
		"total":           total,
		"healthy":         healthy,
		"quota_remaining": st.QuotaRemaining,
		"quota_total":     st.QuotaTotal,
		"account":         st,
	})
}

// models 模型列表：动态拉取（带缓存与静态兜底，见 upstream.Models）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ModelsTimeout)
	defer cancel()
	items, err := h.cfg.Upstream.Models(ctx, nil)
	if err != nil && len(items) == 0 {
		if h.cfg.Kernels != nil && kernel.Installed() {
			raw, _ := json.Marshal(map[string]any{"id": "marvis", "object": "model", "owned_by": "marvis"})
			items = append(items, upstream.ModelInfo{ID: "marvis", RawJSON: raw})
		} else {
			writeOpenAIError(w, http.StatusBadGateway, "模型列表拉取失败: "+err.Error(), "upstream_error")
			return
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	data := make([]json.RawMessage, 0, len(items))
	seen := map[string]bool{}
	for _, mi := range items {
		if seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		if len(mi.RawJSON) > 0 {
			data = append(data, mi.RawJSON)
			continue
		}
		raw, _ := json.Marshal(map[string]any{"id": mi.ID, "object": "model", "owned_by": "marvis2api"})
		data = append(data, raw)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"object":"list","data":` + string(mustMarshal(data)) + `}`))
}

// modelByID 单模型详情：命中目录原始条目原样透传。
func (h *Handler) modelByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeOpenAIError(w, http.StatusNotFound, "model id 为空", "invalid_request_error")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ModelsTimeout)
	defer cancel()
	items, _ := h.cfg.Upstream.Models(ctx, nil)
	for _, mi := range items {
		if mi.ID == id {
			writeRawJSON(w, http.StatusOK, mi.RawJSON)
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "模型不存在: "+id, "invalid_request_error")
}

// ---------------------------------------------------------------------------
// 共用小工具
// ---------------------------------------------------------------------------

// writeJSON JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeRawJSON 预序列化 JSON 响应。
func writeRawJSON(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeOpenAIError OpenAI 风格错误响应（/v1/* 客户端约定形态）。
func writeOpenAIError(w http.ResponseWriter, status int, msg, typ string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": status},
	})
}

// readBody 读取并限长请求体（64MB：多图/长上下文会话以 base64 重发历史时体量可观）。
func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, errEmptyBody
	}
	return raw, nil
}

var errEmptyBody = errStr("请求体为空")

type errStr string

func (e errStr) Error() string { return string(e) }

func mustMarshal(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

func readAllLimit(r io.Reader, limit int64) []byte {
	raw, _ := io.ReadAll(io.LimitReader(r, limit))
	return raw
}

// classifyAndNote 按上游状态码分类记账（chat 用）。
func (h *Handler) classifyAndNote(ue *upstream.Error) state.ErrCategory {
	switch ue.Kind {
	case upstream.ErrAuthDead:
		h.cfg.State.NoteError(state.ErrAuth, 0, time.Time{}, ue.Msg)
		return state.ErrAuth
	case upstream.ErrHardQuota:
		resetAt, _ := upstream.ParseRateReset(ue.Msg)
		h.cfg.State.NoteError(state.ErrQuota, 0, resetAt, "quota exhausted")
		return state.ErrQuota
	case upstream.ErrSoftRate:
		resetAt, _ := upstream.ParseRateReset(ue.Msg)
		if ue.RetryAfter > 0 {
			resetAt = time.Now().Add(ue.RetryAfter)
		}
		h.cfg.State.NoteError(state.ErrRate, h.softCooldown(), resetAt, "rate limited")
		return state.ErrRate
	case upstream.ErrNotFound:
		h.cfg.State.NoteError(state.ErrNotFound, 0, time.Time{}, "upstream 404")
		return state.ErrNotFound
	default:
		h.cfg.State.NoteError(state.ErrServer, 0, time.Time{}, ue.Msg)
		return state.ErrServer
	}
}
