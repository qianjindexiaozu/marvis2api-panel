package oauth

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCheckLoginUsesInjectedKeyAndSkipsMissingKey(t *testing.T) {
	const key = "fixture-oauth-signing-key"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		sum := md5.Sum([]byte(string(body) + r.Header.Get("Ual-Access-Timestamp") + key + r.Header.Get("Ual-Access-Nonce")))
		if r.Header.Get("Ual-Access-Signature") != hex.EncodeToString(sum[:]) {
			t.Error("check_login must use the supplied key")
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"user_info":{"access_token":"activated-token","expires_in":7200}}`)
	}))
	defer srv.Close()
	old := landingBase
	landingBase = srv.URL
	t.Cleanup(func() { landingBase = old })
	jar, _ := cookiejar.New(nil)
	cred := &Credential{AccessToken: "initial-token", OpenID: "fixture-user", RefreshToken: "refresh-token"}
	s := &Session{cred: cred}
	f := NewFlow()
	f.checkLoginQuietly(context.Background(), s, jar, cred)
	if hits.Load() != 0 {
		t.Fatal("missing key must not send an unsigned activation request")
	}
	f.AccessKey = key
	f.checkLoginQuietly(context.Background(), s, jar, cred)
	if hits.Load() != 1 || cred.AccessToken != "activated-token" || cred.ExpiresIn != 7200 {
		t.Fatal("signed activation must update the credentials")
	}
}
