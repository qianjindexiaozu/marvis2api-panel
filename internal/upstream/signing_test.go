package upstream

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/ual"
)

func TestSignedRequestsUseInjectedKey(t *testing.T) {
	const key = "fixture-upstream-signing-key"
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		sum := md5.Sum([]byte(string(body) + r.Header.Get("Ual-Access-Timestamp") + key + r.Header.Get("Ual-Access-Nonce")))
		if r.Header.Get("Ual-Access-Signature") != hex.EncodeToString(sum[:]) {
			t.Error("request did not use the injected key")
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/refresh":
			_, _ = io.WriteString(w, `{"code":0,"user_info":{"access_token":"new-token","expires_in":7200}}`)
		case "/marvis_client/marvis_get_user_info":
			_, _ = io.WriteString(w, `{"ret":0,"nick_name":"fixture-name"}`)
		default:
			t.Error("unexpected request path")
		}
	}))
	defer srv.Close()
	up := New()
	up.AccessKey = key
	up.BaseURL = srv.URL
	up.RefreshPath = "/refresh"
	a := &account.Auth{UID: "fixture-user", Token: "old-token", RefreshToken: "refresh-token", Guid: "fixture-guid"}
	if err := up.RefreshToken(context.Background(), a); err != nil || a.TokenValue() != "new-token" {
		t.Fatalf("refresh: %v", err)
	}
	if nick, err := up.UserInfo(context.Background(), a); err != nil || nick != "fixture-name" {
		t.Fatalf("user info: %v", err)
	}
	if hits.Load() != 2 {
		t.Fatal("both signed endpoints must be exercised")
	}
}

func TestMissingSigningKeyDoesNotContactUpstream(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "must not be contacted", http.StatusInternalServerError)
	}))
	defer srv.Close()
	up := New()
	up.BaseURL, up.WalletURL, up.RefreshPath = srv.URL, srv.URL, "/refresh"
	a := &account.Auth{UID: "fixture-user", Token: "token", RefreshToken: "refresh-token", Guid: "fixture-guid"}
	_, quotaErr := up.Quota(context.Background(), a)
	refreshErr := up.RefreshToken(context.Background(), a)
	_, infoErr := up.UserInfo(context.Background(), a)
	for _, err := range []error{quotaErr, refreshErr, infoErr} {
		if !errors.Is(err, ual.ErrNoAccessKey) {
			t.Fatalf("missing key should fail explicitly: %v", err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("missing key must not send any request")
	}
}
