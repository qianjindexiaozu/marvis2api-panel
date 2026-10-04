package kernel

import (
	"bufio"
	"net"
	"testing"
)

func TestPromptAndEvents(t *testing.T) {
	text, model, stream, err := Prompt([]byte(`{"model":"marvis","stream":true,"messages":[{"role":"user","content":"只回复一个字：好"},{"role":"user","content":[{"type":"text","text":"第二句"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if model != "marvis" || !stream || text != "user: 只回复一个字：好\nuser: 第二句" {
		t.Fatalf("prompt=%q model=%q stream=%v", text, model, stream)
	}
	if _, _, _, err := Prompt([]byte(`{"messages":[]}`)); err == nil {
		t.Fatal("空消息应失败")
	}
	d := parseHostEvent(`{"data":{"delta":"好","type":"TEXT_MESSAGE_CONTENT"},"event":"ag_ui_event","type":"event"}`)
	if d.Text != "好" || d.Err != nil {
		t.Fatalf("delta: %+v", d)
	}
	d = parseHostEvent(`{"data":{"type":"RUN_FINISHED"},"event":"ag_ui_event","type":"event"}`)
	if !d.Done {
		t.Fatal("应结束")
	}
	d = parseHostEvent(`{"type":"error","event":"agent.run","message":"尚未连接"}`)
	if d.Err == nil || d.Err.Error() != "尚未连接" {
		t.Fatal(d.Err)
	}
	d = parseHostEvent(`{"event":"gateway.tick","type":"event"}`)
	if d.Text != "" || d.Err != nil || d.Done {
		t.Fatalf("tick 应忽略: %+v", d)
	}
}

func TestWSFrameRoundTrip(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	go func() { _ = writeFrame(left, 0x1, []byte("好")) }()
	fin, op, payload, err := readFrame(bufio.NewReader(right))
	if err != nil {
		t.Fatal(err)
	}
	if !fin || op != 0x1 || string(payload) != "好" {
		t.Fatalf("frame fin=%v op=%d payload=%q", fin, op, payload)
	}
}
