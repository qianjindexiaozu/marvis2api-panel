// account 包单元测试：Auth 编解码/校验/快照方法与 File 落盘往返。
package account

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAuthMarshalUnmarshalRoundTrip 嵌套形态 Marshal→Unmarshal 往返，
// 并核对落盘 JSON 的段结构（auth/account 两段 + guid/note/created_at）。
func TestAuthMarshalUnmarshalRoundTrip(t *testing.T) {
	src := &Auth{
		Name:         "测试号",
		Token:        "at-xxx",
		RefreshToken: "rt-yyy",
		ExpiresAt:    1792994390,
		BaseURL:      "https://override.example.com",
		Guid:         "g-123",
		UID:          "u-123",
		Note:         "备注",
		CreatedAt:    "2026-01-01T00:00:00Z",
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	// 落盘应为嵌套形态（对齐 workbuddy2api 的 auths 约定）。
	var f authFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Auth.AccessToken != "at-xxx" || f.Auth.RefreshToken != "rt-yyy" || f.Auth.ExpiresAt != 1792994390 {
		t.Fatalf("auth 段不符: %+v", f.Auth)
	}
	if f.Auth.BaseURL != "https://override.example.com" {
		t.Fatalf("baseURL 应在 auth 段: %+v", f.Auth)
	}
	if f.Account.UID != "u-123" || f.Account.Nickname != "测试号" {
		t.Fatalf("account 段不符: %+v", f.Account)
	}
	if f.Guid != "g-123" || f.Note != "备注" || f.CreatedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("guid/note/created_at 不符: %+v", f)
	}
	// 再经 UnmarshalJSON 回读，字段应逐项还原。
	var back Auth
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Token != src.Token || back.RefreshToken != src.RefreshToken || back.ExpiresAt != src.ExpiresAt {
		t.Fatalf("回读 auth 段不符: %+v", &back)
	}
	if back.BaseURL != src.BaseURL || back.Name != src.Name || back.UID != src.UID {
		t.Fatalf("回读 account 段不符: %+v", &back)
	}
	if back.Guid != src.Guid || back.Note != src.Note || back.CreatedAt != src.CreatedAt {
		t.Fatalf("回读元信息不符: %+v", &back)
	}
}

// TestAuthUnmarshalFlatCompat 扁平形态兼容读取（顶层 token/refresh_token/...）。
func TestAuthUnmarshalFlatCompat(t *testing.T) {
	raw := []byte(`{"name":"F","token":"T","refresh_token":"R","expires_at":42,` +
		`"base_url":"https://flat.example.com","uid":"u-9","note":"n",` +
		`"created_at":"2026-02-03T04:05:06Z","guid":"g-9"}`)
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if a.Token != "T" || a.RefreshToken != "R" || a.ExpiresAt != 42 {
		t.Fatalf("扁平形态解析不符: token=%q rt=%q exp=%d", a.Token, a.RefreshToken, a.ExpiresAt)
	}
	if a.BaseURL != "https://flat.example.com" || a.Name != "F" || a.UID != "u-9" {
		t.Fatalf("扁平形态元信息不符: base=%q name=%q uid=%q", a.BaseURL, a.Name, a.UID)
	}
	if a.Note != "n" || a.CreatedAt != "2026-02-03T04:05:06Z" || a.Guid != "g-9" {
		t.Fatalf("扁平形态扩展字段不符: note=%q created=%q guid=%q", a.Note, a.CreatedAt, a.Guid)
	}
}

// TestAuthSnapshotMethods TokenValue/RefreshTokenValue 快照读取与 WriteBack 写回
// （空值保留旧值）。
func TestAuthSnapshotMethods(t *testing.T) {
	a := &Auth{Token: "tok-1", RefreshToken: "rt-1"}
	if got := a.TokenValue(); got != "tok-1" {
		t.Fatalf("TokenValue = %q, 期望 tok-1", got)
	}
	if got := a.RefreshTokenValue(); got != "rt-1" {
		t.Fatalf("RefreshTokenValue = %q, 期望 rt-1", got)
	}
	a.WriteBack("tok-2", "rt-2")
	if a.TokenValue() != "tok-2" || a.RefreshTokenValue() != "rt-2" {
		t.Fatalf("WriteBack 未生效: token=%q rt=%q", a.TokenValue(), a.RefreshTokenValue())
	}
	// 空值写回应保留旧值。
	a.WriteBack("", "")
	if a.TokenValue() != "tok-2" || a.RefreshTokenValue() != "rt-2" {
		t.Fatalf("空值 WriteBack 不应覆盖: token=%q rt=%q", a.TokenValue(), a.RefreshTokenValue())
	}
}

// TestAuthClone Clone 应逐项拷贝且与原对象互不影响。
func TestAuthClone(t *testing.T) {
	a := &Auth{
		Name: "N", Token: "T", RefreshToken: "R", ExpiresAt: 7,
		BaseURL: "https://x.example.com", Guid: "g", UID: "u", Note: "n", CreatedAt: "c",
	}
	c := a.Clone()
	if c.Token != "T" || c.RefreshToken != "R" || c.ExpiresAt != 7 || c.BaseURL != "https://x.example.com" ||
		c.Name != "N" || c.UID != "u" || c.Note != "n" || c.CreatedAt != "c" || c.Guid != "g" {
		t.Fatalf("Clone 字段不符: %+v", c)
	}
	// 改副本不影响原件。
	c.WriteBack("T2", "")
	if a.TokenValue() != "T" {
		t.Fatalf("副本写入不应影响原件: token=%q", a.TokenValue())
	}
}

// TestAuthValidate Validate：token 必填、baseURL 须 http(s)、各字段 trim。
func TestAuthValidate(t *testing.T) {
	// 空 token 报错。
	empty := &Auth{Token: " \n\t "}
	if err := empty.Validate(); err == nil {
		t.Fatal("纯空白 token 应拒绝")
	}
	// baseURL 非 http(s) 报错。
	bad := &Auth{Token: "T", BaseURL: "ftp://x.example.com"}
	if err := bad.Validate(); err == nil {
		t.Fatal("非 http(s) baseURL 应拒绝")
	}
	// 合法输入：trim 生效、baseURL 去尾部斜杠。
	a := &Auth{
		Name: " n ", Token: " tok\n", RefreshToken: " r ",
		BaseURL: "https://x.example.com/", Guid: " g ", UID: " u ", Note: " x ",
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Token != "tok" || a.RefreshToken != "r" || a.Name != "n" || a.Note != "x" {
		t.Fatalf("空白未去掉: token=%q refresh=%q name=%q note=%q", a.Token, a.RefreshToken, a.Name, a.Note)
	}
	if a.BaseURL != "https://x.example.com" || a.Guid != "g" || a.UID != "u" {
		t.Fatalf("baseURL/guid/uid 处理不符: base=%q guid=%q uid=%q", a.BaseURL, a.Guid, a.UID)
	}
}

// TestAuthLabel Label 兜底链：Name → UID → marvis；nil 返回 "-"。
func TestAuthLabel(t *testing.T) {
	if got := (*Auth)(nil).Label(); got != "-" {
		t.Fatalf("nil Label = %q, 期望 -", got)
	}
	if got := (&Auth{Name: "号", UID: "u"}).Label(); got != "号" {
		t.Fatalf("Name 优先: %q", got)
	}
	if got := (&Auth{Name: "  ", UID: "u-1"}).Label(); got != "u-1" {
		t.Fatalf("Name 为空白时应取 UID: %q", got)
	}
	if got := (&Auth{}).Label(); got != "marvis" {
		t.Fatalf("兜底应为 marvis: %q", got)
	}
}

// TestFileRoundTrip Save/Load/Exists 往返：0600 权限、目录自动创建、
// CreatedAt 自动补且二次保存不覆盖。
func TestFileRoundTrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "sub", "auth.json")
	f := NewFile(fp)
	if f.Exists() {
		t.Fatal("初始不应存在")
	}
	a := &Auth{Name: "N", Token: "T", RefreshToken: "R", ExpiresAt: 9, UID: "u", Note: "n"}
	if err := f.Save(a); err != nil {
		t.Fatal(err)
	}
	if !f.Exists() {
		t.Fatal("Save 后应存在")
	}
	// 文件权限 0600。
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("权限 = %v, 期望 0600", fi.Mode().Perm())
	}
	// 回读字段一致。
	got, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "T" || got.RefreshToken != "R" || got.ExpiresAt != 9 || got.Name != "N" || got.UID != "u" || got.Note != "n" {
		t.Fatalf("回读不符: %+v", got)
	}
	// CreatedAt 自动补（RFC3339 可解析）。
	if got.CreatedAt == "" {
		t.Fatal("CreatedAt 应自动补")
	}
	if _, err := time.Parse(time.RFC3339, got.CreatedAt); err != nil {
		t.Fatalf("CreatedAt 非 RFC3339: %q", got.CreatedAt)
	}
	// 二次保存保留已有 CreatedAt。
	a2 := got.Clone()
	a2.Token = "T2"
	if err := f.Save(a2); err != nil {
		t.Fatal(err)
	}
	got2, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got2.CreatedAt != got.CreatedAt || got2.Token != "T2" {
		t.Fatalf("二次保存应保留 CreatedAt 且更新 token: created=%q token=%q", got2.CreatedAt, got2.Token)
	}
}

// TestFileLoadMissing 文件不存在是合法初始状态：返回 (nil, nil)。
func TestFileLoadMissing(t *testing.T) {
	f := NewFile(filepath.Join(t.TempDir(), "absent", "auth.json"))
	a, err := f.Load()
	if a != nil || err != nil {
		t.Fatalf("Load 缺失文件 = (%v, %v), 期望 (nil, nil)", a, err)
	}
}

// TestFileLoadBroken 损坏 JSON 与空 token 均返回 error。
func TestFileLoadBroken(t *testing.T) {
	t.Run("损坏JSON", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "auth.json")
		if err := os.WriteFile(fp, []byte(`{"auth": {`), 0600); err != nil {
			t.Fatal(err)
		}
		if a, err := NewFile(fp).Load(); err == nil {
			t.Fatalf("损坏 JSON 应报错, 得到 (%v, nil)", a)
		}
	})
	t.Run("空token", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "auth.json")
		if err := os.WriteFile(fp, []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
		if a, err := NewFile(fp).Load(); err == nil {
			t.Fatalf("token 为空应报错, 得到 (%v, nil)", a)
		}
	})
}

// TestFileDelete Delete 后文件消失，且对不存在的文件幂等。
func TestFileDelete(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "auth.json")
	f := NewFile(fp)
	if err := f.Save(&Auth{Name: "N", Token: "T"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(); err != nil {
		t.Fatal(err)
	}
	if f.Exists() {
		t.Fatal("Delete 后不应存在")
	}
	// 幂等：再删一次不报错。
	if err := f.Delete(); err != nil {
		t.Fatalf("Delete 应幂等: %v", err)
	}
}
