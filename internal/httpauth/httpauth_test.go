package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyBearer(t *testing.T) {
	mk := func(authz string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		return r
	}
	// 空 key = 不鉴权。
	if !VerifyBearer(mk(""), "") {
		t.Fatal("空 key 应放行")
	}
	if !VerifyBearer(mk("Bearer k1"), "k1") {
		t.Fatal("正确 key 应通过")
	}
	if VerifyBearer(mk("Bearer k2"), "k1") {
		t.Fatal("错误 key 应拒绝")
	}
	if VerifyBearer(mk(""), "k1") {
		t.Fatal("缺头应拒绝")
	}
	if VerifyBearer(mk("Basic k1"), "k1") {
		t.Fatal("方案不对应拒绝")
	}
}
