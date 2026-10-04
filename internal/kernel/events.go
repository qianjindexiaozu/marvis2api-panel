package kernel

import (
	"encoding/json"
	"errors"
	"strings"
)

// errEmptyPrompt 请求里没有可发送的文本。
var errEmptyPrompt = errors.New("messages 里没有可发送的文本")

// Delta 内核往外吐的一块结果。
type Delta struct {
	Text   string
	Err    error
	Done   bool
	Prompt int64
	Compl  int64
}

type agEvent struct {
	Type    string `json:"type"`
	Event   string `json:"event"`
	Message string `json:"message"`
	Data    struct {
		Type        string `json:"type"`
		Delta       string `json:"delta"`
		OK          *bool  `json:"ok"`
		AgentStatus string `json:"agentConnectionStatus"`
		Error       struct {
			Message string `json:"message"`
		} `json:"error"`
		RawEvent struct {
			TokenUsage struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"token_usage"`
		} `json:"rawEvent"`
	} `json:"data"`
}

// parseHostEvent 把 Host WebSocket 的一条 JSON 分成文本、结束、错误或忽略。
func parseHostEvent(raw string) Delta {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, "gateway.tick") {
		return Delta{}
	}
	var ev agEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return Delta{}
	}
	if ev.Type == "error" {
		msg := ev.Message
		if msg == "" {
			msg = "内核返回错误"
		}
		return Delta{Err: errors.New(msg)}
	}
	if ev.Data.OK != nil && !*ev.Data.OK {
		msg := ev.Data.Error.Message
		if msg == "" {
			msg = "内核拒绝了这次请求"
		}
		return Delta{Err: errors.New(msg)}
	}
	switch ev.Data.Type {
	case "TEXT_MESSAGE_CONTENT":
		if ev.Data.Delta == "" {
			return Delta{}
		}
		return Delta{Text: ev.Data.Delta}
	case "TEXT_MESSAGE_END":
		return Delta{Prompt: ev.Data.RawEvent.TokenUsage.InputTokens, Compl: ev.Data.RawEvent.TokenUsage.OutputTokens}
	case "RUN_FINISHED":
		return Delta{Done: true}
	case "RUN_ERROR":
		msg := ev.Data.Error.Message
		if msg == "" {
			msg = ev.Message
		}
		if msg == "" {
			msg = "内核运行失败"
		}
		return Delta{Err: errors.New(msg)}
	}
	if ev.Event == "gateway.connected" && ev.Data.AgentStatus != "" && ev.Data.AgentStatus != "Ready" {
		return Delta{Err: errors.New("账号内核还没就绪（" + ev.Data.AgentStatus + "）")}
	}
	return Delta{}
}
