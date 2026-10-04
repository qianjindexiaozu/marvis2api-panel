// oauth 包测试：用 httptest 假微信/假落地页驱动全流程（无需真实扫码），
// 并覆盖 cookie 收取、状态机与参数形态的关键断言。
package oauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// withFakeEndpoints 把外部端点指到测试假服务，返回还原函数。
func withFakeEndpoints(t *testing.T, qrPage, qrImg, lp, landing string) {
	t.Helper()
	oQRPage, oQRImage, oLP, oBase := wxQRPage, wxQRImage, wxLPPoll, landingBase
	wxQRPage, wxQRImage, wxLPPoll, landingBase = qrPage, qrImg, lp, landing
	t.Cleanup(func() {
		wxQRPage, wxQRImage, wxLPPoll, landingBase = oQRPage, oQRImage, oLP, oBase
	})
}

func TestStartParsesUUIDAndDownloadsQR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect/qrconnect":
			if r.URL.Query().Get("appid") != wxAppID {
				t.Errorf("qrconnect 应带 appid=%s", wxAppID)
			}
			if !strings.Contains(r.URL.Query().Get("redirect_uri"), landingPath+"?login_type=WX") {
				t.Errorf("redirect_uri 形态不符: %s", r.URL.Query().Get("redirect_uri"))
			}
			_, _ = w.Write([]byte(`<img src="/connect/qrcode/UUID123">`))
		case "/connect/qrcode/UUID123":
			_, _ = w.Write([]byte("fake-png-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withFakeEndpoints(t, srv.URL+"/connect/qrconnect", srv.URL+"/connect/qrcode/", srv.URL+"/l/qrconnect", srv.URL+"/marvis_client_login/marvis_oauth")

	f := NewFlow()
	s, err := f.Start(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.uuid != "UUID123" {
		t.Fatalf("uuid 解析错误: %q", s.uuid)
	}
	if string(s.QR) != "fake-png-bytes" {
		t.Fatalf("二维码内容错误: %q", s.QR)
	}
	if s.GUID == "" || len(s.GUID) != 36 {
		t.Fatalf("guid 应为带连字符的 UUID: %q", s.GUID)
	}
}

func TestPollStatusMachineAndExchange(t *testing.T) {
	var sessID atomicStringValue // Start 后由测试主流程写入
	lpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 第一次轮询返回已扫，之后返回确认 + code
		code := "TESTCODE123"
		if n := lpSrvHits.add(1); n == 1 {
			_, _ = fmt.Fprintf(w, "window.wx_errcode=404;window.wx_code='';")
			return
		}
		_, _ = fmt.Fprintf(w, "window.wx_errcode=405;window.wx_code='%s';", code)
	}))
	defer lpSrv.Close()

	landingHits := 0
	var gotCookie, gotCode, gotState string
	landSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/marvis_client_login/marvis_oauth" {
			// check_login 等其他端点：返回成功空响应即可
			_, _ = w.Write([]byte(`{"code":0,"user_info":null}`))
			return
		}
		landingHits++
		q := r.URL.Query()
		gotCode, gotState = q.Get("code"), q.Get("state")
		gotCookie = r.Header.Get("Cookie")
		// 注意：不带 Domain（host-only）——测试在 127.0.0.1 上，域约束 cookie 会被 jar 拒收；
		// 生产环境服务端下发 Domain=.qq.com，对 yybadaccess 域有效。
		http.SetCookie(w, &http.Cookie{Name: "accesstoken", Value: "AT-NEW", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "refreshtoken", Value: "RT-NEW", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "openid", Value: "OID-1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "logintype", Value: "WX", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "expires_in", Value: "7200", Path: "/"})
		w.Header().Set("Location", "https://static.pc.yyb.qq.com/login/login_result.html")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte(`{"code":0,"msg":"RC_SUCCESS"}`))
	}))
	defer landSrv.Close()

	qrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/connect/qrconnect" {
			_, _ = w.Write([]byte(`<img src="/connect/qrcode/U1">`))
			return
		}
		_, _ = w.Write([]byte("qr"))
	}))
	defer qrSrv.Close()
	withFakeEndpoints(t, qrSrv.URL+"/connect/qrconnect", qrSrv.URL+"/connect/qrcode/", lpSrv.URL, landSrv.URL)

	f := NewFlow()
	s, err := f.Start(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	sessID.set(s.ID)

	// 第一轮：已扫码
	st, err := f.Poll(context.Background(), s.ID)
	if err != nil || st != StatusScanned {
		t.Fatalf("第一轮应 scanned, got %s err=%v", st, err)
	}
	// 第二轮：确认 + 换证
	st, err = f.Poll(context.Background(), s.ID)
	if err != nil || st != StatusDone {
		t.Fatalf("第二轮应 done, got %s err=%v", st, err)
	}
	if landingHits != 1 {
		t.Fatalf("落地页应只调一次, got %d", landingHits)
	}
	if !strings.Contains(gotCookie, "guid="+s.GUID) {
		t.Fatalf("落地请求必须带 guid cookie，got %q", gotCookie)
	}
	if gotCode != "TESTCODE123" || gotState != s.GUID {
		t.Fatalf("code/state 参数错误: %q %q", gotCode, gotState)
	}
	cred := f.Credential(s.ID)
	if cred == nil {
		t.Fatal("done 后凭证不应为 nil")
	}
	if cred.AccessToken != "AT-NEW" || cred.RefreshToken != "RT-NEW" || cred.OpenID != "OID-1" {
		t.Fatalf("凭证收取错误: %+v", cred)
	}
	if cred.LoginType != "WX" || cred.GUID != s.GUID || cred.ExpiresIn != 7200 {
		t.Fatalf("凭证字段错误: %+v", cred)
	}
	// 终态幂等：再次轮询直接返回 done
	if st, _ := f.Poll(context.Background(), s.ID); st != StatusDone {
		t.Fatalf("终态应保持 done, got %s", st)
	}
}

func TestPollUnknownSession(t *testing.T) {
	f := NewFlow()
	if _, err := f.Poll(context.Background(), "nope"); err == nil {
		t.Fatal("未知会话应报错")
	}
}

func TestExchangeNonZeroCode(t *testing.T) {
	landSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-109,"msg":"RC_PARAMS_INVALID"}`))
	}))
	defer landSrv.Close()
	withFakeEndpoints(t, "http://127.0.0.1:1/x", "http://127.0.0.1:1/x", "http://127.0.0.1:1/x", landSrv.URL)

	f := NewFlow()
	s := &Session{ID: "s1", GUID: "g1", CreatedAt: time.Now(), stat: StatusWaiting}
	// exchange 本身只返回错误；错误文本写入会话由 Poll 的调用处负责。
	if err := f.exchange(context.Background(), s, "CODE"); err == nil {
		t.Fatal("code!=0 应报错")
	} else if !strings.Contains(err.Error(), "-109") {
		t.Fatalf("错误应包含业务码: %v", err)
	}
}

func TestHarvestCookies(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://yybadaccess.3g.qq.com/x")
	jar.SetCookies(u, []*http.Cookie{
		{Name: "accesstoken", Value: "a"},
		{Name: "refreshtoken", Value: "r"},
		{Name: "openid", Value: "o"},
		{Name: "logintype", Value: "wx"},
		{Name: "expires_in", Value: "7200"},
		{Name: "unrelated", Value: "x"},
	})
	c := harvestCookies(jar, u.String())
	if c.AccessToken != "a" || c.RefreshToken != "r" || c.OpenID != "o" {
		t.Fatalf("收取错误: %+v", c)
	}
	if c.LoginType != "WX" { // 大写归一
		t.Fatalf("logintype 应大写: %q", c.LoginType)
	}
	if c.ExpiresIn != 7200 {
		t.Fatalf("expires_in 解析错误: %d", c.ExpiresIn)
	}
}

// atomicStringValue 简单的并发安全字符串（测试用）。
type atomicStringValue struct {
	mu sync.Mutex
	v  string
}

func (a *atomicStringValue) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicStringValue) get() string  { a.mu.Lock(); v := a.v; a.mu.Unlock(); return v }

// counter 简单计数器（测试用）。
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) add(d int) int { c.mu.Lock(); c.n += d; v := c.n; c.mu.Unlock(); return v }

var lpSrvHits = &counter{}
