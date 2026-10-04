package upstream

import (
	"strings"
	"testing"
)

func TestParseNick(t *testing.T) {
	nick, ok := parseNick([]byte(`{"ret":0,"nick_name":"微信昵称"}`))
	if !ok || nick != "微信昵称" {
		t.Fatalf("nick=%q ok=%v", nick, ok)
	}
	nick, ok = parseNick([]byte(`{"code":0,"user_info":{"nickname":"QQ%E5%90%8D"}}`))
	if !ok || nick != "QQ名" {
		t.Fatalf("decoded=%q ok=%v", nick, ok)
	}
	if _, ok := parseNick([]byte(`{"ret":1,"msg":"no"}`)); ok {
		t.Fatal("失败响应不应取出名称")
	}
	if err := userInfoErr([]byte(`{"ret":-101,"msg":"get third cache err:redis: nil","nick_name":""}`)); err == nil || !strings.Contains(err.Error(), "redis") {
		t.Fatal(err)
	}
}
