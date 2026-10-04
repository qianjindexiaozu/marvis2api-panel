package kernel

import (
	"encoding/json"
	"strings"
)

// Prompt 从 OpenAI chat body 抽出要发给内核的文本、模型名和是否流式。
func Prompt(body []byte) (text, model string, stream bool, err error) {
	var obj struct {
		Model    string            `json:"model"`
		Stream   bool              `json:"stream"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err = json.Unmarshal(body, &obj); err != nil {
		return "", "", false, err
	}
	var b strings.Builder
	for _, raw := range obj.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		part := contentText(m.Content)
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		role := m.Role
		if role == "" {
			role = "user"
		}
		b.WriteString(role)
		b.WriteString(": ")
		b.WriteString(part)
	}
	text = strings.TrimSpace(b.String())
	if text == "" {
		return "", "", false, errEmptyPrompt
	}
	return text, obj.Model, obj.Stream, nil
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}
