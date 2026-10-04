package upstream

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
)

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"402 quota", 402, `{"error":"no credit"}`, ErrHardQuota},
		{"401 auth", 401, `{"msg":"unauthorized"}`, ErrAuthDead},
		{"403 auth", 403, `<html>forbidden</html>`, ErrAuthDead},
		{"429 rate", 429, `{"msg":"quota exceeded 请稍后"}`, ErrSoftRate},
		{"200 body quota keyword", 400, `{"msg":"额度不足，请充值"}`, ErrHardQuota},
		{"non-429 rate keyword", 400, `{"msg":"rate limited, retry later"}`, ErrSoftRate},
		{"prompt too long", 400, `{"msg":"maximum context length exceeded"}`, ErrPromptTooLong},
		{"404", 404, `not found`, ErrNotFound},
		{"500", 500, `internal error`, ErrServer},
		{"content blocked", 400, `{"msg":"Content blocked by security policy"}`, ErrContentBlocked},
		{"plain 400", 400, `{"msg":"bad field"}`, ErrClient},
		{"200 ok", 200, `{"choices":[]}`, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d, %q) = %v, want %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	if _, ok := ParseRetryAfter(h); ok {
		t.Fatal("空头不应解析成功")
	}
	h.Set("Retry-After", "30")
	if d, ok := ParseRetryAfter(h); !ok || d != 30*time.Second {
		t.Fatalf("Retry-After=30 应得 30s，得到 %v ok=%v", d, ok)
	}
	h.Del("Retry-After")
	h.Set("Retry-After-Ms", "1500")
	if d, ok := ParseRetryAfter(h); !ok || d != 1500*time.Millisecond {
		t.Fatalf("Retry-After-Ms=1500 应得 1.5s，得到 %v", d)
	}
	h.Del("Retry-After-Ms")
	h.Set("X-Ratelimit-Reset", "1")
	// epoch=1 已在过去：remain 为负 → 丢弃。
	if _, ok := ParseRetryAfter(h); ok {
		t.Fatal("过去时刻应丢弃")
	}
	// 异常大（> sanity 2h）丢弃。
	h.Del("X-Ratelimit-Reset")
	h.Set("Retry-After", "100000")
	if _, ok := ParseRetryAfter(h); ok {
		t.Fatal("超上限应丢弃")
	}
}

func TestParseRateReset(t *testing.T) {
	tm, ok := ParseRateReset(`429 {"msg":"使用量超限，将在 2026-09-27 04:00:00 重置"}`)
	if !ok || tm.IsZero() {
		t.Fatalf("中文重置文案应解析成功，得到 %v ok=%v", tm, ok)
	}
	tm2, ok := ParseRateReset(`429 {"msg":"will reset at 2026-09-27 04:00:00"}`)
	if !ok {
		t.Fatalf("英文重置文案应解析成功")
	}
	if !tm.Equal(tm2) {
		t.Fatalf("两种形态应解析出同一时刻：%v vs %v", tm, tm2)
	}
	if _, ok := ParseRateReset("no time here"); ok {
		t.Fatal("无时间文案应失败")
	}
}

func TestPrepareBodySanitize(t *testing.T) {
	c := New()
	c.SetExtraFingerprints([]string{"SecretCLI"})
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"use marvis2api with SecretCLI please"},{"role":"assistant","content":"ok marvis.qq.com"},{"role":"system","content":"keep marvis.qq.com intact"}]}`)
	out := string(c.PrepareBody(body))
	if contains(out, "marvis2api") || contains(out, "SecretCLI") {
		t.Fatalf("user/assistant 指纹应被替换：%s", out)
	}
	if !contains(out, "keep marvis.qq.com intact") {
		t.Fatalf("system 应保持原样：%s", out)
	}
	// 关闭脱敏：完全还原。
	c.SanitizeFingerprints = false
	if out2 := string(c.PrepareBody(body)); !contains(out2, "marvis2api") {
		t.Fatalf("关闭脱敏应原样返回：%s", out2)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestParseModels(t *testing.T) {
	items, err := parseModels([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	if err != nil || len(items) != 2 {
		t.Fatalf("data 形态解析失败: %v %v", items, err)
	}
	items, err = parseModels([]byte(`[{"id":"x"}]`))
	if err != nil || len(items) != 1 || items[0].ID != "x" {
		t.Fatalf("顶层数组形态解析失败: %v %v", items, err)
	}
	if _, err := parseModels([]byte(`{}`)); err == nil {
		t.Fatal("空目录应报错")
	}
}

func TestParseQuota(t *testing.T) {
	q, err := parseQuota([]byte(`{"remaining":100,"total":200}`))
	if err != nil || q.Remaining != 100 || q.Total != 200 {
		t.Fatalf("直返形态解析失败: %+v %v", q, err)
	}
	q, err = parseQuota([]byte(`{"code":0,"data":{"remaining":5,"total":10}}`))
	if err != nil || q.Remaining != 5 || q.Total != 10 {
		t.Fatalf("信封形态解析失败: %+v %v", q, err)
	}
	if _, err := parseQuota([]byte(`garbage`)); err == nil {
		t.Fatal("垃圾 body 应报错")
	}
}

func TestParseWallet(t *testing.T) {
	raw := []byte(`{"code":0,"msg":"","data":{"total_tokens":"10000000","used_tokens":"6660","avail_tokens":"9993340","frozen_tokens":"0","status":1}}`)
	q, err := parseWallet(raw)
	if err != nil || q.Remaining != 9993340 || q.Total != 10000000 {
		t.Fatalf("钱包额度解析失败: %+v %v", q, err)
	}
	if _, err := parseWallet([]byte(`{"code":0,"data":null}`)); err == nil {
		t.Fatal("data 为空应报错")
	}
}

func TestWalletQuotaUsesAvailTokens(t *testing.T) {
	const testAccessKey = "test-wallet-key"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Ual-Access-Access-Token")
		if r.Header.Get("Ual-Access-Guid") != "device-guid" || r.Header.Get("Ual-Access-Openid") != "oid-1" {
			http.Error(w, "missing identity", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		sum := md5.Sum(append(append(append(body, []byte(r.Header.Get("Ual-Access-Timestamp"))...), []byte(testAccessKey)...), []byte(r.Header.Get("Ual-Access-Nonce"))...))
		if hex.EncodeToString(sum[:]) != r.Header.Get("Ual-Access-Signature") {
			http.Error(w, "bad sig", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"total_tokens":"10000000","avail_tokens":"9993340"}}`))
	}))
	defer srv.Close()

	up := New()
	up.AccessKey = testAccessKey
	up.WalletURL = srv.URL
	up.QuotaPath = ""
	q, err := up.Quota(context.Background(), &account.Auth{UID: "oid-1", Token: "tok-secret", Guid: "device-guid"})
	if err != nil || q.Remaining != 9993340 || q.Total != 10000000 {
		t.Fatalf("应读到剩余额度: %+v %v", q, err)
	}
	if gotAuth != "tok-secret" {
		t.Fatal("请求应带上账号 token")
	}
	if _, err := up.Quota(context.Background(), &account.Auth{UID: "oid-1", Token: "tok-secret"}); err != ErrNoGUID {
		t.Fatalf("没有 guid 应返回 ErrNoGUID，得到 %v", err)
	}
}
