// 出站 body 准备：指纹脱敏（黑名单词替换）。
package upstream

import (
	"encoding/json"
	"strings"
)

// defaultFingerprints 出站脱敏的默认黑名单词（大小写不敏感子串替换）。
// 目标：客户端 SDK / CLI 注入的固定模板句里可能携带本网关与上游服务的指纹串，
// 上游内容审核按词匹配时可能误杀合法流量——出站前统一替换为中性词。
// 默认词表保持最小（宿主域名 / 客户端名），配置可追加。
var defaultFingerprints = []string{
	"marvis.qq.com",
	"marvis2api",
	"Marvis 桌面助手",
}

// PrepareBody 出站前对客户端 body 做指纹脱敏 + gpt 系参数兼容：
//
//   - SanitizeFingerprints 开启时，把 messages 里 user / assistant 文本中的
//     黑名单指纹串替换为 "[filtered]"（system 不动——网关承诺不注入自有 system，
//     客户端 system 内容按原样传递，由调用方自行选择提示词模式）；
//   - model 为 gpt-*（上游转发到 OpenAI 原生通道）时，把 max_tokens 改写为
//     max_completion_tokens——OpenAI 新接口不认 max_tokens，直接 400
//     invalid parameter（实测 gpt-5.4，2026-09-27）；
//   - 关闭时原样返回（完全还原）。
//
// 解析失败时原样返回（不做任何改写：坏 body 交给上游报 400，语义更诚实）。
func (c *Client) PrepareBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	if c.SanitizeFingerprints {
		body = c.sanitizeBody(body)
	}
	return rewriteGPTMaxTokens(body)
}

// sanitizeBody 指纹脱敏（原 PrepareBody 主体）。
func (c *Client) sanitizeBody(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgsRaw, ok := obj["messages"]
	if !ok {
		return body
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return body
	}
	changed := false
	repl := c.replacementMap()
	for i, m := range msgs {
		role := rawRole(m["role"])
		if role != "user" && role != "assistant" {
			continue
		}
		if txt, ok := rawText(m["content"]); ok {
			if out, hit := replaceAll(txt, repl); hit {
				msgs[i]["content"], _ = json.Marshal(out)
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	outMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	obj["messages"] = outMsgs
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// rewriteGPTMaxTokens gpt 系模型的 max_tokens → max_completion_tokens 改写。
// model 前缀 "gpt" 且 body 带 max_tokens 时改写键名（值不变）；
// 已带 max_completion_tokens 时不覆盖。解析失败原样返回。
func rewriteGPTMaxTokens(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	mdl := rawModelLower(obj["model"])
	if !strings.HasPrefix(mdl, "gpt") {
		return body
	}
	mt, hasMT := obj["max_tokens"]
	if !hasMT {
		return body
	}
	if _, hasMCT := obj["max_completion_tokens"]; hasMCT {
		return body
	}
	obj["max_completion_tokens"] = mt
	delete(obj, "max_tokens")
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// rawModelLower 提取 model 字段并转小写（缺失/非字符串返回 ""）。
func rawModelLower(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.ToLower(s)
}

// replacementMap 生效的脱敏替换表（默认词 ∪ 额外词），小写键匹配原文大小写变体。
func (c *Client) replacementMap() map[string]string {
	c.modelsMu.RLock()
	defer c.modelsMu.RUnlock()
	repl := make(map[string]string, len(defaultFingerprints)+len(c.extraFingerprints))
	for _, fp := range defaultFingerprints {
		if fp != "" {
			repl[strings.ToLower(fp)] = fp
		}
	}
	for _, fp := range c.extraFingerprints {
		if fp != "" {
			repl[strings.ToLower(fp)] = fp
		}
	}
	return repl
}

// replaceAll 把 s 中命中的指纹串（大小写不敏感）替换为 "[filtered]"。
// 返回替换结果与是否发生替换。
func replaceAll(s string, repl map[string]string) (string, bool) {
	lower := strings.ToLower(s)
	hitAny := false
	for key := range repl {
		if strings.Contains(lower, key) {
			hitAny = true
			break
		}
	}
	if !hitAny {
		return s, false
	}
	// 逐词替换（词数有限，直接循环；大小写不敏感需手工扫描）。
	out := s
	for key, orig := range repl {
		out = replaceInsensitive(out, key)
		_ = orig
	}
	return out, true
}

// replaceInsensitive 大小写不敏感地把 src 中所有 key 替换为 "[filtered]"。
func replaceInsensitive(src, key string) string {
	if key == "" {
		return src
	}
	var b strings.Builder
	lower := strings.ToLower(src)
	k := strings.ToLower(key)
	for {
		idx := strings.Index(lower, k)
		if idx < 0 {
			b.WriteString(src)
			return b.String()
		}
		b.WriteString(src[:idx])
		b.WriteString("[filtered]")
		src = src[idx+len(key):]
		lower = lower[idx+len(k):]
	}
}

// rawRole 提取消息 role 字符串。
func rawRole(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// rawText 提取文本形态 content（string）；数组形态（多模态 parts）不动——
// 只脱敏纯文本轮，图片/工具等结构化 part 交由上游自行处理。
func rawText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
