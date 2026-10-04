package apikey

import "testing"

func TestMaskKeepsEnds(t *testing.T) {
	got := Mask("mv-abcdef1234567890wxyz")
	if got != "mv-a****wxyz" {
		t.Fatalf("mask=%s", got)
	}
}

func TestCreateAndAccept(t *testing.T) {
	s := NewStore(t.TempDir() + "/keys.json")
	k, err := s.Create("客户端")
	if err != nil || k.Secret == "" {
		t.Fatal(err)
	}
	if !s.Accept(k.Secret) {
		t.Fatal("新密钥应通过")
	}
	if s.Accept("nope") {
		t.Fatal("错误密钥不应通过")
	}
	if !s.SetDisabled(k.ID, true) || s.Accept(k.Secret) {
		t.Fatal("停用后不应通过")
	}
	again := NewStore(s.path)
	if again.Accept(k.Secret) {
		t.Fatal("重启后停用状态应还在")
	}
}
