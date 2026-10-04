// panel_test.go 面板包测试：登录会话与安全响应头、单账号凭证运维
// （保存/删除/探活/配额/刷新/复活）、overview 形态、config 读写与网关代理。
package panel

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/gateway"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/livecfg"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/metrics"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/scheduler"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/usage"
)

// setup 构建单账号装配：状态机（空路径 = 不落盘）+ auth.json 凭证文件 +
// 上游客户端 + 调度器。返回面板、状态机与凭证文件，供测试直接断言内部状态。
func setup(t *testing.T) (*Panel, *state.State, *account.File) {
	t.Helper()
	st := state.New("")
	file := account.NewFile(filepath.Join(t.TempDir(), "auth.json"))
	up := upstream.New()
	up.AccessKey = "test-panel-signing-key"
	pn := New(Config{
		State:        st,
		File:         file,
		Usage:        usage.New(""),
		APIKey:       "panelkey",
		Version:      "test",
		Live:         livecfg.New(livecfg.Snapshot{APIKey: "panelkey"}),
		Upstream:     up,
		Sched:        scheduler.New(scheduler.Config{State: st, Upstream: up, File: file, Timeout: 5 * time.Second}),
		ProbeTimeout: 5 * time.Second,
	})
	return pn, st, file
}

// do 以 Bearer api_key 发起一次面板请求。
func do(t *testing.T, pn *Panel, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	pn.ServeHTTP(w, req)
	return w
}

// loginAs 走真实登录接口拿会话 Cookie。
func loginAs(t *testing.T, pn *Panel, password string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, pn, "POST", "/panel/api/login", "", `{"password":"`+password+`"}`)
}

func TestPanelAuthRequired(t *testing.T) {
	pn, _, _ := setup(t)
	w := do(t, pn, "GET", "/panel/api/overview", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应 401，得到 %d", w.Code)
	}
	w = do(t, pn, "GET", "/panel/api/overview", "wrong", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误密钥应 401，得到 %d", w.Code)
	}
}

func TestPanelSecurityHeaders(t *testing.T) {
	pn, _, _ := setup(t)
	w := do(t, pn, "GET", "/panel/", "panelkey", "")
	for _, h := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy"} {
		if w.Header().Get(h) == "" {
			t.Fatalf("缺少安全响应头 %s", h)
		}
	}
}

func TestLoginSessionFlow(t *testing.T) {
	pn, _, _ := setup(t)

	// 未登录：session 探测匿名可达且 authenticated=false；受保护 API 401。
	w := do(t, pn, "GET", "/panel/api/session", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatalf("session 探测应 200/未登录，得到 %d %s", w.Code, w.Body.String())
	}
	if w = do(t, pn, "GET", "/panel/api/overview", "", ""); w.Code != 401 {
		t.Fatalf("未登录访问受保护 API 应 401，得到 %d", w.Code)
	}

	// 错误密码 401。
	if w = loginAs(t, pn, "nope"); w.Code != 401 {
		t.Fatalf("错误密码应 401，得到 %d", w.Code)
	}

	// 正确密码 200 + HttpOnly 会话 Cookie；Cookie 直接受保护 API 放行。
	w = loginAs(t, pn, "panelkey")
	if w.Code != 200 {
		t.Fatalf("正确密码应 200，得到 %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value == "" || !cookies[0].HttpOnly {
		t.Fatalf("应颁发 HttpOnly 会话 Cookie，得到 %v", cookies)
	}
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.AddCookie(cookies[0])
	w = httptest.NewRecorder()
	pn.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("会话 Cookie 应放行 overview，得到 %d", w.Code)
	}

	// Bearer 会话 token 同样放行（脚本场景）；Bearer api_key 兼容保留。
	token := cookies[0].Value
	w = do(t, pn, "GET", "/panel/api/overview", token, "")
	if w.Code != 200 {
		t.Fatalf("Bearer 会话 token 应放行，得到 %d", w.Code)
	}

	// 注销后 Cookie 立即失效。
	w = do(t, pn, "POST", "/panel/api/logout", token, "")
	if w.Code != 200 {
		t.Fatalf("logout 应 200，得到 %d", w.Code)
	}
	w = do(t, pn, "GET", "/panel/api/overview", token, "")
	if w.Code != 401 {
		t.Fatalf("注销后应 401，得到 %d", w.Code)
	}
}

func TestLoginBruteForceLockout(t *testing.T) {
	pn, _, _ := setup(t)
	for i := 0; i < 10; i++ {
		if w := loginAs(t, pn, "nope"); w.Code != 401 {
			t.Fatalf("第 %d 次错误密码应 401，得到 %d", i+1, w.Code)
		}
	}
	// 达上限后：即使密码正确也锁定。
	if w := loginAs(t, pn, "panelkey"); w.Code != 401 {
		t.Fatalf("锁定窗口内正确密码也应 401，得到 %d", w.Code)
	}
}

func TestLoginFreeAuthMode(t *testing.T) {
	// api_key 为空 = 免鉴权模式：login 拒绝，受保护 API 直接放行。
	pn := New(Config{
		State: state.New(""),
		File:  account.NewFile(filepath.Join(t.TempDir(), "auth.json")),
		Usage: usage.New(""),
		Live:  livecfg.New(livecfg.Snapshot{}),
	})
	if w := loginAs(t, pn, "x"); w.Code != http.StatusBadRequest {
		t.Fatalf("免鉴权模式登录应 400，得到 %d", w.Code)
	}
	if w := do(t, pn, "GET", "/panel/api/overview", "", ""); w.Code != 200 {
		t.Fatalf("免鉴权模式应放行 overview，得到 %d", w.Code)
	}
}

func TestPanelDefaultPasswordFlag(t *testing.T) {
	pn, _, _ := setup(t) // api_key = panelkey（非默认）
	w := do(t, pn, "GET", "/panel/api/session", "", "")
	if strings.Contains(w.Body.String(), `"using_default_password":true`) {
		t.Fatalf("自定义密钥不应报默认密码: %s", w.Body.String())
	}
	// 默认密码实例。
	p2 := New(Config{
		State: state.New(""),
		File:  account.NewFile(filepath.Join(t.TempDir(), "auth.json")),
		Usage: usage.New(""),
		Live:  livecfg.New(livecfg.Snapshot{APIKey: DefaultPassword}),
	})
	w = do(t, p2, "GET", "/panel/api/session", "", "")
	if !strings.Contains(w.Body.String(), `"using_default_password":true`) {
		t.Fatalf("默认密码应被标记: %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 单账号凭证运维
// ---------------------------------------------------------------------------

// 未配置凭证是合法初始状态：观测接口可读，操作接口一律 404 提示先保存 token。
func TestCredentialEndpointsWithoutCredential(t *testing.T) {
	pn, st, _ := setup(t)
	for _, path := range []string{
		"/panel/api/credential/probe",
		"/panel/api/credential/quota",
		"/panel/api/credential/refresh-token",
	} {
		w := do(t, pn, "POST", path, "panelkey", "")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s 未配置凭证应 404，得到 %d %s", path, w.Code, w.Body.String())
		}
	}
	if st.HasAuth() {
		t.Fatal("不应有凭证")
	}
	// 观测接口仍然可读。
	w := do(t, pn, "GET", "/panel/api/credential", "panelkey", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"has_token":false`) {
		t.Fatalf("credential GET 应 200 且 has_token=false，得到 %d %s", w.Code, w.Body.String())
	}
}

// 保存凭证：校验 → 原子落盘 auth.json → 立即进状态机；token 不回传。
func TestCredentialSaveRoundTrip(t *testing.T) {
	pn, st, file := setup(t)

	w := do(t, pn, "PUT", "/panel/api/credential", "panelkey",
		`{"name":"A1","token":"tok-1","refresh_token":"ref-1","uid":"u1","guid":"g1","note":"n1"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("保存应 200，得到 %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tok-1") || strings.Contains(w.Body.String(), "ref-1") {
		t.Fatal("保存响应不得回传 token 明文")
	}
	if !st.HasAuth() {
		t.Fatal("保存后状态机应有凭证")
	}
	a, err := file.Load()
	if err != nil || a == nil {
		t.Fatalf("auth.json 应已落盘: %v", err)
	}
	if a.TokenValue() != "tok-1" || a.RefreshTokenValue() != "ref-1" {
		t.Fatalf("落盘内容不符: token=%q refresh=%q", a.TokenValue(), a.RefreshTokenValue())
	}

	// GET：has_token/has_file 布尔透出，无明文。
	w = do(t, pn, "GET", "/panel/api/credential", "panelkey", "")
	if w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"has_token":true`) ||
		!strings.Contains(w.Body.String(), `"has_file":true`) {
		t.Fatalf("credential GET 应含 has_token/has_file，得到 %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "tok-1") {
		t.Fatal("credential GET 不得回传 token 明文")
	}

	// 再次保存不带 token：保留原 token，只改名称。
	w = do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"name":"A1-改名"}`)
	if w.Code != 200 {
		t.Fatalf("二次保存应 200，得到 %d %s", w.Code, w.Body.String())
	}
	a, _ = file.Load()
	if a.TokenValue() != "tok-1" || a.Name != "A1-改名" {
		t.Fatalf("留空 token 应保留原值并更新名称: token=%q name=%q", a.TokenValue(), a.Name)
	}

	// 校验失败：全新实例没有 token 可保留 → 400。
	pn2, _, _ := setup(t)
	if w = do(t, pn2, "PUT", "/panel/api/credential", "panelkey", `{"name":"X"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("无 token 保存应 400，得到 %d %s", w.Code, w.Body.String())
	}
	// base_url 非法 → 400。
	if w = do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"token":"tok-2","base_url":"ftp://x"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 base_url 应 400，得到 %d %s", w.Code, w.Body.String())
	}
}

// 换凭证应当自动复活：认证失效硬冷却后，人工换 token 保存即恢复 healthy，
// 否则还得再点一次「解除冷却」，反直觉。凭证没变就不许动状态。
func TestCredentialSaveRevivesOnTokenChange(t *testing.T) {
	t.Run("换 access token", func(t *testing.T) {
		pn, st, _ := setup(t)
		st.SetAuth(&account.Auth{Name: "C1", Token: "tok", RefreshToken: "ref"})
		st.NoteError(state.ErrAuth, 0, time.Time{}, "登录态过期")
		if got := st.Status(); got.State != "cooling" {
			t.Fatalf("前置条件：应处于硬冷却，得到 %s", got.State)
		}
		w := do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"token":"tok-new"}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"revived":true`) {
			t.Fatalf("换 token 应标记 revived，得到 %d %s", w.Code, w.Body.String())
		}
		got := st.Status()
		if got.State != "healthy" || !got.Until.IsZero() {
			t.Fatalf("换 token 后应复活且清零冷却，得到 state=%s until=%v", got.State, got.Until)
		}
	})
	t.Run("凭证未变不清冷却", func(t *testing.T) {
		pn, st, _ := setup(t)
		st.SetAuth(&account.Auth{Name: "C1", Token: "tok"})
		st.NoteError(state.ErrAuth, 0, time.Time{}, "登录态过期")
		w := do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"name":"改名而已","token":"tok"}`)
		if w.Code != 200 {
			t.Fatalf("保存应 200，得到 %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), `"revived":true`) {
			t.Fatalf("凭证未变不得标记复活: %s", w.Body.String())
		}
		if got := st.Status(); got.State != "cooling" {
			t.Fatalf("凭证未变时应保持冷却，得到 %s", got.State)
		}
	})
}

// 删除凭证：文件移除 + 状态机回未配置；文件不存在时幂等。
func TestCredentialDelete(t *testing.T) {
	pn, st, file := setup(t)
	if w := do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"token":"tok-1"}`); w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, pn, "DELETE", "/panel/api/credential", "panelkey", ""); w.Code != 200 {
		t.Fatalf("删除应 200，得到 %d %s", w.Code, w.Body.String())
	}
	if file.Exists() {
		t.Fatal("删除后 auth.json 应不存在")
	}
	if st.HasAuth() || st.Status().Configured {
		t.Fatal("删除后状态机应为未配置")
	}
	if w := do(t, pn, "DELETE", "/panel/api/credential", "panelkey", ""); w.Code != 200 {
		t.Fatalf("重复删除应幂等 200，得到 %d %s", w.Code, w.Body.String())
	}
}

// 手动复活：清冷却 + 熔断（panel 路径 + admin 别名双通道）。
func TestCredentialRevive(t *testing.T) {
	pn, st, _ := setup(t)
	st.SetAuth(&account.Auth{Name: "R1", Token: "tok"})
	for _, path := range []string{"/panel/api/credential/revive", "/admin/revive"} {
		st.NoteError(state.ErrAuth, 0, time.Time{}, "登录态过期")
		if got := st.Status(); got.State != "cooling" {
			t.Fatalf("前置条件：应处于冷却，得到 %s", got.State)
		}
		w := do(t, pn, "POST", path, "panelkey", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
			t.Fatalf("%s 应 200，得到 %d %s", path, w.Code, w.Body.String())
		}
		if got := st.Status(); got.State != "healthy" {
			t.Fatalf("%s 后应恢复 healthy，得到 %s", path, got.State)
		}
	}
}

// overview 总览形态：单账号计数 + account 快照 + 不回传 token 明文。
func TestOverviewShape(t *testing.T) {
	pn, _, _ := setup(t)
	w := do(t, pn, "GET", "/panel/api/overview", "panelkey", "")
	if w.Code != 200 {
		t.Fatalf("overview 应 200，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"total":1`) || !strings.Contains(w.Body.String(), `"healthy":0`) {
		t.Fatalf("未配置时 total=1 healthy=0: %s", w.Body.String())
	}

	if w = do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"name":"A1","token":"tok-1"}`); w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	w = do(t, pn, "GET", "/panel/api/overview", "panelkey", "")
	body := w.Body.String()
	if !strings.Contains(body, `"healthy":1`) || !strings.Contains(body, `"account"`) || !strings.Contains(body, `"name":"A1"`) {
		t.Fatalf("配置后 overview 应 healthy=1 且带 account 快照: %s", body)
	}
	if !strings.Contains(body, `"has_token":true`) {
		t.Fatalf("account 快照应带 has_token 布尔: %s", body)
	}
	if strings.Contains(body, "tok-1") {
		t.Fatal("overview 不得回传 token 明文")
	}
}

// 探活与配额：走假上游，结果同步返回并记账进状态机。
func TestCredentialQuotaAndProbe(t *testing.T) {
	pn, st, _ := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
		case "/quota":
			_, _ = w.Write([]byte(`{"remaining":7,"total":9}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	pn.cfg.Upstream.BaseURL = srv.URL
	pn.cfg.Upstream.QuotaPath = "/quota"

	if w := do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"token":"tok-1","guid":"g1"}`); w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}

	w := do(t, pn, "POST", "/panel/api/credential/quota", "panelkey", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) ||
		!strings.Contains(w.Body.String(), `"remaining":7`) || !strings.Contains(w.Body.String(), `"total":9`) {
		t.Fatalf("配额查询应成功，得到 %d %s", w.Code, w.Body.String())
	}
	if got := st.Status(); got.QuotaRemaining != 7 || got.QuotaTotal != 9 {
		t.Fatalf("配额应记账进状态机，得到 remaining=%d total=%d", got.QuotaRemaining, got.QuotaTotal)
	}

	w = do(t, pn, "POST", "/panel/api/credential/probe", "panelkey", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"m1"`) {
		t.Fatalf("探活应成功，得到 %d %s", w.Code, w.Body.String())
	}
	if got := st.Status(); !got.LastProbeOK {
		t.Fatal("探活成功应记账进状态机")
	}
}

// refresh-token 刷新：路径未配置 501；无 refresh_token 400；成功后新 token
// 立即落盘并进状态机；失败时原 token 保留；响应一律不回传 token。
func TestCredentialRefreshToken(t *testing.T) {
	pn, st, file := setup(t)
	if w := do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"token":"tok-only","uid":"openid-1","guid":"guid-1"}`); w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}

	// 未配置刷新路径 → 501，token 不动。
	if w := do(t, pn, "POST", "/panel/api/credential/refresh-token", "panelkey", ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("未配置 refresh_path 应 501，得到 %d %s", w.Code, w.Body.String())
	}

	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"user_info":{"access_token":"new-tok","refresh_token":"new-r","expires_in":7200}}`))
	}))
	defer srv.Close()
	pn.cfg.Upstream.BaseURL = srv.URL
	pn.cfg.Upstream.RefreshPath = "/refresh"

	// 没有 refresh_token → 400。
	if w := do(t, pn, "POST", "/panel/api/credential/refresh-token", "panelkey", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("没有 refresh_token 应 400，得到 %d %s", w.Code, w.Body.String())
	}

	// 补上 refresh_token 后刷新成功。
	if w := do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"refresh_token":"old-r"}`); w.Code != 200 {
		t.Fatalf("补存 refresh_token 失败: %d %s", w.Code, w.Body.String())
	}
	w := do(t, pn, "POST", "/panel/api/credential/refresh-token", "panelkey", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("刷新应成功，得到 %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "new-tok") || strings.Contains(w.Body.String(), "new-r") {
		t.Fatal("刷新响应不得回传 token")
	}
	a, err := file.Load()
	if err != nil || a.TokenValue() != "new-tok" || a.RefreshTokenValue() != "new-r" {
		t.Fatalf("新 token 应落盘: %v", err)
	}
	if got := st.Auth(); got == nil || got.TokenValue() != "new-tok" {
		t.Fatal("新 token 应立即进状态机")
	}

	// 刷新失败：ok:false，原 token 保留。
	fail = true
	w = do(t, pn, "POST", "/panel/api/credential/refresh-token", "panelkey", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("上游失败应 ok:false，得到 %d %s", w.Code, w.Body.String())
	}
	if a, _ = file.Load(); a.TokenValue() != "new-tok" {
		t.Fatalf("失败刷新不得改 token，得到 %q", a.TokenValue())
	}
}

// 配额刷新是后台异步：HTTP 请求返回（ctx 取消）后上游查询仍要跑完并记账。
func TestRefreshQuotaOutlivesRequest(t *testing.T) {
	pn, st, _ := setup(t)
	if w := do(t, pn, "PUT", "/panel/api/credential", "panelkey", `{"token":"tok-1"}`); w.Code != 200 {
		t.Fatalf("保存失败: %d %s", w.Code, w.Body.String())
	}
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-r.Context().Done():
			http.Error(w, "canceled", http.StatusTeapot)
		case <-time.After(250 * time.Millisecond):
			_, _ = w.Write([]byte(`{"remaining":7,"total":9}`))
		}
	}))
	defer srv.Close()
	pn.cfg.Upstream.BaseURL = srv.URL
	pn.cfg.Upstream.QuotaPath = "/quota"

	if w := do(t, pn, "POST", "/panel/api/refresh", "panelkey", ""); w.Code != 200 {
		t.Fatalf("刷新应 200，得到 %d %s", w.Code, w.Body.String())
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("配额刷新没有打到上游")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := st.Status(); got.QuotaRemaining == 7 && got.QuotaTotal == 9 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := st.Status()
	t.Fatalf("请求返回后配额应写入状态机，得到 remaining=%d total=%d", got.QuotaRemaining, got.QuotaTotal)
}

// ---------------------------------------------------------------------------
// config 读写与网关代理
// ---------------------------------------------------------------------------

// config 读写：未装配回调 501；注入假实现后 GET/POST 走回调且可计数验证。
func TestConfigEndpoints(t *testing.T) {
	pn, _, _ := setup(t)

	// 未装配 → 501（装配可选）。
	if w := do(t, pn, "GET", "/panel/api/config", "panelkey", ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("未装配 loader 应 501，得到 %d", w.Code)
	}
	if w := do(t, pn, "POST", "/panel/api/config", "panelkey", `{}`); w.Code != http.StatusNotImplemented {
		t.Fatalf("未装配 saver 应 501，得到 %d", w.Code)
	}

	// 注入假实现（可计数）。
	pn.cfg.LoadConfig = func() (any, error) {
		return map[string]any{"listen": ":18620"}, nil
	}
	var posts [][]byte
	pn.cfg.SaveConfig = func(raw []byte) ([]string, error) {
		posts = append(posts, raw)
		return []string{"auth_file", "listen"}, nil
	}

	w := do(t, pn, "GET", "/panel/api/config", "panelkey", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"listen":":18620"`) {
		t.Fatalf("config GET 应回 loader 结果，得到 %d %s", w.Code, w.Body.String())
	}

	raw := `{"listen":":18620","api_key":"newkey"}`
	w = do(t, pn, "POST", "/panel/api/config", "panelkey", raw)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restart_required"`) {
		t.Fatalf("config POST 应 200 且带 restart_required，得到 %d %s", w.Code, w.Body.String())
	}
	if len(posts) != 1 || string(posts[0]) != raw {
		t.Fatalf("SaveConfig 应收到原始请求体恰好 1 次，得到 %d 次 %q", len(posts), posts)
	}

	// 保存失败 → 400，请求体不重复计数。
	pn.cfg.SaveConfig = func(raw []byte) ([]string, error) { return nil, errors.New("配置不合法") }
	if w = do(t, pn, "POST", "/panel/api/config", "panelkey", raw); w.Code != http.StatusBadRequest {
		t.Fatalf("SaveConfig 报错应 400，得到 %d %s", w.Code, w.Body.String())
	}
	if len(posts) != 1 {
		t.Fatalf("失败调用不应计入成功数，得到 %d", len(posts))
	}
}

// Gateway 未注入时聊天代理 501（装配可选）。
func TestChatProxyGatewayNil(t *testing.T) {
	pn, _, _ := setup(t)
	w := do(t, pn, "POST", "/panel/api/chat", "panelkey", `{"model":"m","messages":[]}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("Gateway nil 应 501，得到 %d %s", w.Code, w.Body.String())
	}
}

func TestPanelStatsProxy(t *testing.T) {
	pn, _, _ := setup(t) // 未注入 Metrics → 回落 metrics.Global（与 gateway 同语义）
	metrics.Global().Record("deepseek-v4", true, 200, time.Second, 5, 7)
	w := do(t, pn, "GET", "/panel/api/stats", "panelkey", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"models"`) {
		t.Fatalf("stats 代理应 200 且含 models，得到 %d %s", w.Code, w.Body.String())
	}
	if w = do(t, pn, "POST", "/panel/api/stats/reset", "panelkey", ""); w.Code != 200 {
		t.Fatalf("stats reset 代理应 200，得到 %d", w.Code)
	}
}

// proxyFake 模拟 OpenAI 兼容上游（SSE 可编程），并记录收到的上游鉴权头。
type proxyFake struct {
	sse   bool
	auths []string
}

func (f *proxyFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.auths = append(f.auths, r.Header.Get("Ual-Access-Access-Token"))
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":7,"completion_tokens":1}}`))
}

func TestPanelChatProxySSE(t *testing.T) {
	fu := &proxyFake{sse: true}
	srv := httptest.NewServer(fu)
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.BaseURL = srv.URL
	up.HTTP.Timeout = 5 * time.Second
	up.ChatHTTP.Timeout = 5 * time.Second
	up.SetHeaderTimeout(5 * time.Second)

	// 同进程网关 + 面板（真实装配形态：Gateway 直调注入），单账号进状态机。
	st := state.New("")
	st.SetAuth(&account.Auth{Name: "A1", Token: "tok1"})
	gh := gateway.New(gateway.Config{
		State:         st,
		Upstream:      up,
		APIKey:        "panelkey",
		SoftCooldown:  time.Second,
		ModelsTimeout: 5 * time.Second,
		Metrics:       metrics.New(),
	})
	pn := New(Config{
		State:    st,
		File:     account.NewFile(filepath.Join(t.TempDir(), "auth.json")),
		Usage:    usage.New(""),
		APIKey:   "panelkey",
		Live:     livecfg.New(livecfg.Snapshot{APIKey: "panelkey"}),
		Gateway:  gh,
		Upstream: up,
	})

	// 登录拿会话 Cookie → 经代理发起流式对话。
	w := loginAs(t, pn, "panelkey")
	if w.Code != 200 {
		t.Fatalf("登录应 200，得到 %d", w.Code)
	}
	cookie := w.Result().Cookies()[0]

	body := `{"model":"deepseek-v4","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/panel/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("聊天代理应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"he"`) || !strings.Contains(out, `"content":"llo"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("SSE 应原样透传: %s", out)
	}
	// 分层断言：面板注入 api_key 过网关鉴权（否则 401）→ 网关注入账号 token 调上游。
	// 上游看到的是 Ual-Access-Access-Token（账号 token），不是 api_key——凭证分层正确。
	if len(fu.auths) == 0 || fu.auths[0] != "tok1" {
		t.Fatalf("上游应收到网关注入的账号 token，得到 %v", fu.auths)
	}
}

// ---------------------------------------------------------------------------
// 包内小件
// ---------------------------------------------------------------------------

func TestRingChannelAndCap(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 5; i++ {
		_, _ = r.Write([]byte("chat status=200 test\n"))
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("环形缓冲应保留最近 3 条，得到 %d", len(snap))
	}
	if snap[0].Channel != "chat" {
		t.Fatalf("chat 行应归 chat 频道，得到 %s", snap[0].Channel)
	}
}

func TestPublicErrHidesHTML(t *testing.T) {
	err := &upstream.Error{Kind: upstream.ErrAuthDead, Status: 404, Msg: "<!DOCTYPE html><html>secret-token-value-abcdefghij</html>"}
	got := publicErr(err)
	if strings.Contains(got, "secret-token") || strings.Contains(strings.ToLower(got), "<html") {
		t.Fatalf("HTML 错误页不应进面板提示: %s", got)
	}
	if got != "上游返回 HTTP 404" {
		t.Fatalf("应只留状态码，得到 %s", got)
	}
}

// 前端约束：不允许内联事件处理（CSP script-src 'self' 会拦掉，按钮会失灵）。
func TestNoInlineEventHandlers(t *testing.T) {
	for _, name := range []string{"index.html", "app.js"} {
		raw, err := webFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if s := string(raw); strings.Contains(s, "onclick=") || strings.Contains(s, "onsubmit=") {
			t.Fatalf("%s 含内联事件处理，会被 CSP script-src 'self' 拦掉，按钮点击没有反应", name)
		}
	}
}
