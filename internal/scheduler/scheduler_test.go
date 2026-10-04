// scheduler 包的单元测试：后台任务开关组合、无凭证静默跳过与 keepalive 刷新链路。
package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
)

// TestBackgroundTasksOn 三开关（配额/保活/探测）全部组合：任一开启即调度，全关即空转。
func TestBackgroundTasksOn(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"全部关闭", Config{}, false},
		{"仅配额", Config{QuotaRefreshMinutes: 5}, true},
		{"仅保活", Config{KeepaliveHours: []int{3}}, true},
		{"仅探测", Config{ProbeIntervalSeconds: 30}, true},
		{"配额加保活", Config{QuotaRefreshMinutes: 5, KeepaliveHours: []int{3}}, true},
		{"配额加探测", Config{QuotaRefreshMinutes: 5, ProbeIntervalSeconds: 30}, true},
		{"保活加探测", Config{KeepaliveHours: []int{3}, ProbeIntervalSeconds: 30}, true},
		{"三项全开", Config{QuotaRefreshMinutes: 5, KeepaliveHours: []int{3}, ProbeIntervalSeconds: 30}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := New(tc.cfg).backgroundTasksOn(); got != tc.want {
				t.Fatalf("backgroundTasksOn()=%v, want %v", got, tc.want)
			}
		})
	}
}

// hitServer 计数假上游：任何请求都被计数（且不返回有效业务数据），
// 用于断言「不应被请求」。
func hitServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestRoundsSkipWithoutAuth 未配置凭证（State 未注入 / 无凭证 / token 为空）时，
// roundQuota 与 roundProbe 必须静默跳过：不 panic、不打上游、不写状态。
func TestRoundsSkipWithoutAuth(t *testing.T) {
	cases := []struct {
		name  string
		build func() *state.State // 返回 nil = 不注入 State
	}{
		{"state未注入", func() *state.State { return nil }},
		{"state无凭证", func() *state.State { return state.New("") }},
		{"凭证token为空", func() *state.State {
			st := state.New("")
			// 只有 refresh_token、没有 access token，视为未配置。
			st.SetAuth(&account.Auth{RefreshToken: "r"})
			return st
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := hitServer(t)
			up := upstream.New()
			up.BaseURL = srv.URL
			up.QuotaPath = "/quota"
			up.ModelsPath = "/models" // 配置端点让探测走真实 HTTP，若未跳过即命中假上游
			st := tc.build()
			s := New(Config{State: st, Upstream: up, Timeout: 2 * time.Second})

			s.roundQuota(context.Background())
			s.roundProbe(context.Background())

			if n := hits.Load(); n != 0 {
				t.Fatalf("未配置凭证时不应请求上游，实际请求 %d 次", n)
			}
			if st != nil {
				sp := st.Status()
				if !sp.QuotaUpdatedAt.IsZero() || !sp.LastProbeAt.IsZero() {
					t.Fatalf("跳过时不应写状态: quota_at=%v probe_at=%v", sp.QuotaUpdatedAt, sp.LastProbeAt)
				}
			}
		})
	}
}

// TestRoundKeepaliveSkips 未配置刷新端点或上游时，保活必须静默跳过：
// 不打上游、不改凭证、不落盘。
func TestRoundKeepaliveSkips(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "auth.json")
	f := account.NewFile(fp)
	a := &account.Auth{Name: "K", Token: "tok", RefreshToken: "r"}
	if err := f.Save(a); err != nil {
		t.Fatal(err)
	}
	st := state.New("")
	st.SetAuth(a)

	t.Run("RefreshPath为空", func(t *testing.T) {
		srv, hits := hitServer(t)
		up := upstream.New()
		up.BaseURL = srv.URL // RefreshPath 缺省为空：即使 BaseURL 指向假上游也不应请求
		s := New(Config{State: st, Upstream: up, File: f, Timeout: 2 * time.Second})
		s.roundKeepalive(context.Background())

		if n := hits.Load(); n != 0 {
			t.Fatalf("RefreshPath 为空时不应请求上游，实际请求 %d 次", n)
		}
		if got := st.Auth(); got == nil || got.TokenValue() != "tok" {
			t.Fatalf("凭证不应被改写, got %+v", got)
		}
	})

	t.Run("upstream未注入", func(t *testing.T) {
		s := New(Config{State: st, File: f, Timeout: 2 * time.Second})
		s.roundKeepalive(context.Background()) // 不 panic 即可
	})
}

// TestKeepaliveRefreshReachesStateAndFile 保活主链路：假 upstream 返回新 token，
// 刷新结果应同时写进状态机与 auth.json 落盘（沿用旧测试的 httptest 手法）。
func TestKeepaliveRefreshReachesStateAndFile(t *testing.T) {
	var gotRefresh, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			UserInfo struct {
				OpenID       string `json:"openId"`
				RefreshToken string `json:"refreshToken"`
			} `json:"userInfo"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRefresh = body.UserInfo.RefreshToken
		gotCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte(`{"code":0,"msg":"RC_SUCCESS","user_info":{"access_token":"new-tok","refresh_token":"new-r","expires_in":7200}}`))
	}))
	defer srv.Close()

	f := account.NewFile(filepath.Join(t.TempDir(), "auth.json"))
	a := &account.Auth{Name: "K", Token: "old-tok", RefreshToken: "old-r", UID: "openid-1", Guid: "guid-1"}
	if err := f.Save(a); err != nil {
		t.Fatal(err)
	}
	st := state.New("")
	st.SetAuth(a)

	up := upstream.New()
	up.AccessKey = "test-scheduler-key"
	up.BaseURL = srv.URL
	up.RefreshPath = "/refresh"
	s := New(Config{State: st, Upstream: up, File: f, Timeout: 2 * time.Second})
	s.roundKeepalive(context.Background())

	if gotRefresh != "old-r" {
		t.Fatalf("刷新请求应携带旧 refresh_token, got %q", gotRefresh)
	}
	if !strings.Contains(gotCookie, "openid=openid-1") || !strings.Contains(gotCookie, "guid=guid-1") {
		t.Fatalf("刷新请求应携带 openid/guid Cookie, got %q", gotCookie)
	}
	// 状态机内立即可见新 token。
	cur := st.Auth()
	if cur == nil || cur.TokenValue() != "new-tok" || cur.RefreshTokenValue() != "new-r" {
		t.Fatalf("刷新后的 token 应写入状态机, got %+v", cur)
	}
	// 落盘文件同步更新。
	disk, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if disk.TokenValue() != "new-tok" || disk.RefreshTokenValue() != "new-r" {
		t.Fatalf("刷新后的 token 应落盘, got token=%q refresh=%q",
			disk.TokenValue(), disk.RefreshTokenValue())
	}
}

func TestNeedsRefreshTenMinutes(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	soon := &account.Auth{Token: "t", RefreshToken: "r", UID: "u"}
	soon.SetExpiresAt(now.Add(9 * time.Minute).Unix())
	later := &account.Auth{Token: "t", RefreshToken: "r", UID: "u"}
	later.SetExpiresAt(now.Add(11 * time.Minute).Unix())
	if !needsRefresh(soon, now) {
		t.Fatal("9 分钟内应刷新")
	}
	if needsRefresh(later, now) {
		t.Fatal("11 分钟后不应刷新")
	}
	if needsRefresh(&account.Auth{Token: "t", UID: "u"}, now) {
		t.Fatal("没有 refresh token 不应刷新")
	}
}

func TestRefreshDueSkipsAccountNotNearExpiry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"user_info":{"access_token":"new","expires_in":7200}}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	store := account.NewStore(dir)
	now := time.Now()
	due := &account.Auth{Token: "old", RefreshToken: "r", UID: "due-user", Guid: "g1", LoginType: "WX"}
	due.SetExpiresAt(now.Add(5 * time.Minute).Unix())
	wait := &account.Auth{Token: "old2", RefreshToken: "r2", UID: "wait-user", Guid: "g2", LoginType: "WX"}
	wait.SetExpiresAt(now.Add(30 * time.Minute).Unix())
	if err := store.Save(due); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(wait); err != nil {
		t.Fatal(err)
	}
	up := upstream.New()
	up.AccessKey = "test-scheduler-key"
	up.BaseURL = srv.URL
	up.RefreshPath = "/marvis_client/marvis_refresh_token"
	s := New(Config{Upstream: up, Accounts: store, Timeout: 2 * time.Second})
	s.refreshDue(context.Background(), now)
	if hits.Load() != 1 {
		t.Fatalf("只应刷新临近过期的账号, hits=%d", hits.Load())
	}
	got, err := store.Load("due-user")
	if err != nil || got.TokenValue() != "new" {
		t.Fatalf("临近过期的账号应写回新 token, err=%v", err)
	}
	other, err := store.Load("wait-user")
	if err != nil || other.TokenValue() != "old2" {
		t.Fatal("未到期账号不应被改")
	}
}
