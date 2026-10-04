package account

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreKeepsTwoAccounts(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	a := &Auth{Token: "t1", UID: "user-a", Guid: "g1", LoginType: "WX", Name: "甲"}
	b := &Auth{Token: "t2", UID: "user-b", Guid: "g2", LoginType: "WX", Name: "乙"}
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(b); err != nil {
		t.Fatal(err)
	}
	if err := s.SetActive(b.ID()); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 个账号，得到 %d", len(list))
	}
	if s.Active() != "user-b" {
		t.Fatalf("active=%s", s.Active())
	}
	// 同号再登录只更新，不新增。
	a.Token = "t1b"
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List()
	if len(list) != 2 {
		t.Fatalf("更新后仍应是 2 个，得到 %d", len(list))
	}
	got, err := s.Load("user-a")
	if err != nil || got.TokenValue() != "t1b" {
		t.Fatalf("load %+v %v", got, err)
	}
	if err := s.Delete("user-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "user-a.json")); !os.IsNotExist(err) {
		t.Fatal("删除后文件还在")
	}
}
