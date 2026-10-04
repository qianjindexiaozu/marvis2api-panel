package gateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
)

func TestRoutePicksHigherQuotaAndSticks(t *testing.T) {
	store := account.NewStore(t.TempDir())
	low := &account.Auth{Token: "t", UID: "low", Guid: "g1"}
	high := &account.Auth{Token: "t", UID: "high", Guid: "g2"}
	if err := store.Save(low); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(high); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: Config{Accounts: store}, routes: newRouter()}
	h.routes.setQuota("low", 100)
	h.routes.setQuota("high", 10)
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	got, err := h.chooseAccount(context.Background(), "", body, nil)
	if err != nil || got.ID() != "low" {
		t.Fatalf("新对话应选剩余更多的账号, got %v %v", got, err)
	}
	h.routes.setQuota("low", 1)
	h.routes.setQuota("high", 500)
	got, err = h.chooseAccount(context.Background(), "", body, nil)
	if err != nil || got.ID() != "low" {
		t.Fatalf("同一段对话应留在原账号, got %v %v", got, err)
	}
	other := []byte(`{"messages":[{"role":"user","content":"another chat"}]}`)
	got, err = h.chooseAccount(context.Background(), "", other, nil)
	if err != nil || got.ID() != "high" {
		t.Fatalf("新对话应改选剩余更多的账号, got %v %v", got, err)
	}
	got, err = h.chooseAccount(context.Background(), "low", other, nil)
	if err != nil || got.ID() != "low" {
		t.Fatalf("请求头应优先, got %v %v", got, err)
	}
}

func TestSessionKeyPrefersConversationID(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("X-Marvis-Session", "from-header")
	if got := sessionKey(hdr, []byte(`{"conversation_id":"from-body"}`)); got != "h:from-header" {
		t.Fatalf("header key = %q", got)
	}
	if got := sessionKey(nil, []byte(`{"metadata":{"conversationId":"abc"}}`)); got != "c:abc" {
		t.Fatalf("metadata key = %q", got)
	}
	a := sessionKey(nil, []byte(`{"messages":[{"role":"user","content":"same"}]}`))
	b := sessionKey(nil, []byte(`{"messages":[{"role":"user","content":"same"},{"role":"assistant","content":"x"}]}`))
	if a == "" || a != b {
		t.Fatalf("首条用户消息应稳定, %q %q", a, b)
	}
}
