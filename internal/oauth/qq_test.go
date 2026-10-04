package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestParsePtuiAndHash33(t *testing.T) {
	code, jump, msg := parsePtui(`ptuiCB('66','0','','0','二维码未失效。', '')`)
	if code != "66" || jump != "" || msg != "二维码未失效。" {
		t.Fatalf("waiting: %q %q %q", code, jump, msg)
	}
	code, jump, msg = parsePtui(`ptuiCB('0','0','https://ptlogin2.qq.com/check_sig?x=1','0','登录成功！', 'n')`)
	if code != "0" || jump != "https://ptlogin2.qq.com/check_sig?x=1" || msg != "登录成功！" {
		t.Fatalf("ok: %q %q %q", code, jump, msg)
	}
	if parsePtuiNick(`ptuiCB('0','0','https://ptlogin2.qq.com/check_sig?x=1','0','登录成功！', 'n')`) != "n" {
		t.Fatal("应取出第 6 个参数作为昵称")
	}
	if hash33("") != 0 {
		t.Fatal("empty hash")
	}
	if hash33("abc") == 0 {
		t.Fatal("hash collapsed")
	}
	if NormalizeLoginType("1") != "QC" || NormalizeLoginType("2") != "WX" || NormalizeLoginType("qc") != "QC" {
		t.Fatal("login type")
	}
}

func TestGTKAndOAuthCode(t *testing.T) {
	if gtk("") != 5381 || gtk("a") != 177670 {
		t.Fatalf("gtk empty=%d a=%d", gtk(""), gtk("a"))
	}
	raw := `{"ret":0,"callback":"https://yybadaccess.3g.qq.com/marvis_client_login/marvis_oauth?login_type=QC&code=abc&state=s"}`
	if extractOAuthCode(raw) != "abc" {
		t.Fatalf("code = %q", extractOAuthCode(raw))
	}
}

func TestCompleteQQCodePostsAuthorize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jump" {
			_, _ = io.WriteString(w, `<html><script>postMessage("qclogin_success")</script></html>`)
			return
		}
		if r.Method != http.MethodPost || r.FormValue("from_ptlogin") != "1" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"ret":0,"callback":"https://yybadaccess.3g.qq.com/marvis_client_login/marvis_oauth?code=from-post&state=g"}`)
	}))
	defer srv.Close()
	old := qqAuthorizeURL
	qqAuthorizeURL = srv.URL + "/authorize"
	t.Cleanup(func() { qqAuthorizeURL = old })
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("https://graph.qq.com/")
	jar.SetCookies(u, []*http.Cookie{{Name: "p_skey", Value: "abc", Path: "/"}})
	client := &http.Client{Jar: jar}
	code, err := completeQQCode(context.Background(), client, srv.URL+"/jump", "guid-1")
	if err != nil || code != "from-post" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestQQStartReturnsWaitingQR(t *testing.T) {
	if os.Getenv("MV2A_LIVE_TESTS") != "1" {
		t.Skip("set MV2A_LIVE_TESTS=1 to contact the real QQ login service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	f := NewQQFlow()
	s, err := f.Start(ctx)
	if err != nil {
		t.Skip(err)
	}
	if len(s.QR) < 8 {
		t.Fatal("empty qr")
	}
	st, err := f.Poll(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st != StatusWaiting && st != StatusScanned {
		_, msg := s.Stat()
		t.Fatalf("status %s %s", st, msg)
	}
}
