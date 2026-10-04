// Package gateway 网关 handler 测试：单账号状态机驱动的鉴权 / 健康检查 /
// chat 转发 / 冷却拒绝 / 并发上限。
//
// 手法沿用旧版：httptest 起假上游，upstream.New 的 BaseURL 指向它；
// state.New("") 内存态（不落盘），SetAuth 注入单账号凭证。
package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/metrics"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
)

// chatPath 假上游的对话端点路径。
const chatPath = "/v1/chat/completions"

// fakeUpstream 模拟 Marvis 上游：可编程按次序返回状态码；真实上游响应
// Content-Type 恒为 text/plain，这里默认也回 text/plain 以贴近真实行为。
type fakeUpstream struct {
	mu         sync.Mutex
	calls      map[string]int // path -> 次数
	statuses   []int          // chat 按次序返回的状态码（index 超界回 200）
	sse        bool           // 200 时是否回 SSE 帧
	ct         string         // 200 时响应 Content-Type（默认 text/plain）
	retryAfter string         // 429 时附带的 Retry-After 头

	// 最近一次 chat 请求观测（加锁读取）。
	tokSeen   string // Ual-Access-Access-Token
	guidSeen  string // Ual-Access-Guid
	sceneSeen string // Marvis-Scene
	reqCT     string // 请求 Content-Type
	body      []byte // 请求原始 body
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls[r.URL.Path]++
	n := f.calls[r.URL.Path]
	if r.URL.Path == chatPath {
		f.tokSeen = r.Header.Get("Ual-Access-Access-Token")
		f.guidSeen = r.Header.Get("Ual-Access-Guid")
		f.sceneSeen = r.Header.Get("Marvis-Scene")
		f.reqCT = r.Header.Get("Content-Type")
		f.body = readAllLimit(r.Body, 1<<20)
	}
	status := http.StatusOK
	if n <= len(f.statuses) {
		status = f.statuses[n-1]
	}
	retryAfter := f.retryAfter
	sse := f.sse
	ct := f.ct
	f.mu.Unlock()

	if status != http.StatusOK {
		if status == http.StatusTooManyRequests && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		http.Error(w, `{"error":{"message":"upstream rejected"}}`, status)
		return
	}
	if ct == "" {
		ct = "text/plain" // 真实上游对话响应恒为 text/plain
	}
	w.Header().Set("Content-Type", ct)
	if r.URL.Path != chatPath {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		return
	}
	if sse {
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return
	}
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`))
}

// chatCalls 返回 chat 端点被调用的次数。
func (f *fakeUpstream) chatCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[chatPath]
}

// newTestUpstream 构建指向假上游的客户端（短超时快速失败；关脱敏以便逐字节比对透传）。
func newTestUpstream(t *testing.T, fu *fakeUpstream) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(fu)
	t.Cleanup(srv.Close)
	up := upstream.New()
	up.BaseURL = srv.URL
	up.HTTP.Timeout = 5 * time.Second
	up.ChatHTTP.Timeout = 5 * time.Second // 测试用：快速失败（生产为 0，靠首字节/空闲兜底）
	up.SetHeaderTimeout(5 * time.Second)
	up.QuotaPath = ""
	up.RefreshPath = ""
	up.SanitizeFingerprints = false
	return up
}

// newHandler 构建被测 handler（静态 api_key，独立 metrics 实例避免全局污染）。
func newHandler(up *upstream.Client, st *state.State) *Handler {
	return New(Config{
		State:         st,
		Upstream:      up,
		APIKey:        "testkey",
		SoftCooldown:  time.Minute,
		ModelsTimeout: 5 * time.Second,
		Metrics:       metrics.New(),
	})
}

// setup 假上游 + 已配置凭证的完整环境；返回 handler / 假上游 / 状态机。
func setup(t *testing.T, statuses []int, sse bool) (*Handler, *fakeUpstream, *state.State) {
	t.Helper()
	fu := &fakeUpstream{calls: map[string]int{}, statuses: statuses, sse: sse}
	up := newTestUpstream(t, fu)
	st := state.New("")
	st.SetAuth(&account.Auth{Name: "A1", Token: "tok1", Guid: "guid-1"})
	return newHandler(up, st), fu, st
}

// postChat 以正确密钥发起一次对话请求。
func postChat(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer testkey")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestChatAuthRequired 错误 / 缺失密钥一律 401。
func TestChatAuthRequired(t *testing.T) {
	h, _, _ := setup(t, nil, false)
	for name, hdr := range map[string]string{
		"缺失密钥": "",
		"错误密钥": "Bearer wrong",
	} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s应 401，得到 %d", name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "invalid_api_key") {
			t.Fatalf("%s错误类型应为 invalid_api_key: %s", name, w.Body.String())
		}
	}
}

// TestNoAPIKeyDisablesAuth api_key 为空 = 不鉴权放行。
func TestNoAPIKeyDisablesAuth(t *testing.T) {
	h, _, _ := setup(t, nil, false)
	h.cfg.APIKey = ""
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("空密钥应放行，得到 %d body=%s", w.Code, w.Body.String())
	}
}

// TestHealthz 未配置凭证 503，配置后 200。
func TestHealthz(t *testing.T) {
	fu := &fakeUpstream{calls: map[string]int{}}
	up := newTestUpstream(t, fu)
	st := state.New("")
	h := newHandler(up, st)

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置凭证应 503，得到 %d", w.Code)
	}

	st.SetAuth(&account.Auth{Name: "A1", Token: "tok1"})
	req = httptest.NewRequest("GET", "/healthz", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("配置凭证后应 200，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"service":"marvis2api"`) {
		t.Fatalf("应带 service 标识: %s", w.Body.String())
	}
}

// TestChatNoAccount 未配置凭证时 503 no_account。
func TestChatNoAccount(t *testing.T) {
	fu := &fakeUpstream{calls: map[string]int{}}
	up := newTestUpstream(t, fu)
	h := newHandler(up, state.New(""))

	w := postChat(t, h, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置凭证应 503，得到 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no_account") {
		t.Fatalf("错误码应为 no_account: %s", w.Body.String())
	}
	if fu.chatCalls() != 0 {
		t.Fatal("未配置凭证不应触达上游")
	}
}

// TestChatForward 配置凭证后正常转发：校验假上游收到的 UAL 头族与 body 透传。
func TestChatForward(t *testing.T) {
	h, fu, _ := setup(t, nil, false)
	sent := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	w := postChat(t, h, sent)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d body=%s", w.Code, w.Body.String())
	}

	// 假上游默认回 text/plain，网关非流式侧应规范为 application/json 透传。
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("非流式响应应为 application/json，得到 %s", got)
	}
	if !strings.Contains(w.Body.String(), `"content":"hello"`) {
		t.Fatalf("上游 body 应原样透传: %s", w.Body.String())
	}

	// 上游收到的请求头：UAL 鉴权头族 + chat 路由头 + JSON CT。
	if fu.tokSeen != "tok1" {
		t.Fatalf("上游应收到账号 token，得到 %q", fu.tokSeen)
	}
	if fu.guidSeen != "guid-1" {
		t.Fatalf("上游应收到 Ual-Access-Guid，得到 %q", fu.guidSeen)
	}
	if fu.sceneSeen != "agent_chat" {
		t.Fatalf("上游应收到 Marvis-Scene=agent_chat，得到 %q", fu.sceneSeen)
	}
	if fu.reqCT != "application/json" {
		t.Fatalf("出站请求应为 JSON CT，得到 %q", fu.reqCT)
	}
	// body 透传（测试客户端已关脱敏，模型非 gpt* 不触发改写 → 应逐字节一致）。
	if !bytes.Equal(fu.body, []byte(sent)) {
		t.Fatalf("请求体应原样透传，得到 %s", fu.body)
	}
}

// TestChatStreamRelay 流式判定看出站 stream 标志：假上游即使回 text/plain，
// stream:true 请求也应按 SSE 透传（真实上游行为）。
func TestChatStreamRelay(t *testing.T) {
	h, _, _ := setup(t, nil, true)
	w := postChat(t, h, `{"model":"m1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("应为 SSE 响应: %s", w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"he"`) || !strings.Contains(body, `"content":"llo"`) {
		t.Fatalf("流式分片应原样透传: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatal("应包含 [DONE]")
	}
}

// TestChat429SoftCooldown 上游 429：账号进入软冷却，响应带 Retry-After；
// 冷却期内再请求被 503 account_unavailable 拒绝。
func TestChat429SoftCooldown(t *testing.T) {
	h, fu, st := setup(t, []int{http.StatusTooManyRequests}, false)
	// 假上游明示 3 秒后重试。
	fu.retryAfter = "3"

	first := postChat(t, h, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if first.Code != http.StatusTooManyRequests {
		t.Fatalf("首次应透传上游 429，得到 %d body=%s", first.Code, first.Body.String())
	}
	if ra := first.Header().Get("Retry-After"); ra == "" {
		t.Fatal("429 响应应带 Retry-After")
	}
	if got := st.Status().State; got != "cooling" {
		t.Fatalf("429 后账号应进入冷却，得到 %s", got)
	}

	second := postChat(t, h, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("冷却期内应 503，得到 %d body=%s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "account_unavailable") {
		t.Fatalf("错误码应为 account_unavailable: %s", second.Body.String())
	}
	if ra := second.Header().Get("Retry-After"); ra == "" {
		t.Fatal("冷却拒绝响应也应带 Retry-After")
	}
}

// TestStatusEndpoint /status 返回账号快照（含凭证观测与配额字段）。
func TestStatusEndpoint(t *testing.T) {
	h, _, st := setup(t, nil, false)
	st.SetQuota(42, 100)

	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer testkey")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status 应 200，得到 %d", w.Code)
	}
	var snap struct {
		Service        string `json:"service"`
		Total          int    `json:"total"`
		Healthy        int    `json:"healthy"`
		QuotaRemaining int64  `json:"quota_remaining"`
		QuotaTotal     int64  `json:"quota_total"`
		Account        struct {
			Configured bool   `json:"configured"`
			State      string `json:"state"`
			Name       string `json:"name"`
			HasToken   bool   `json:"has_token"`
			Guid       string `json:"guid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("status 非法 JSON: %v", err)
	}
	if snap.Service != "marvis2api" || snap.Total != 1 || snap.Healthy != 1 {
		t.Fatalf("汇总字段不符: %+v", snap)
	}
	if snap.QuotaRemaining != 42 || snap.QuotaTotal != 100 {
		t.Fatalf("配额观测不符: %+v", snap)
	}
	a := snap.Account
	if !a.Configured || a.State != "healthy" || a.Name != "A1" || !a.HasToken || a.Guid != "guid-1" {
		t.Fatalf("账号快照不符: %+v", a)
	}
}

// TestMaxInFlightFull 在途占满时 503 in_flight_full；释放后恢复。
func TestMaxInFlightFull(t *testing.T) {
	h, _, st := setup(t, nil, false)
	st.SetMaxInFlight(1)

	// 直接占住唯一的在途名额。
	if !st.Acquire() {
		t.Fatal("占位 Acquire 应成功")
	}
	w := postChat(t, h, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("在途占满应 503，得到 %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "in_flight_full") {
		t.Fatalf("错误码应为 in_flight_full: %s", w.Body.String())
	}

	// 释放后恢复正常转发。
	st.Release()
	w = postChat(t, h, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("释放后应恢复 200，得到 %d body=%s", w.Code, w.Body.String())
	}
}
