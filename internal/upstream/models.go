// 模型目录：真实客户端目录（staticModels）+ 可选动态拉取（models_path 配置了
// 可用端点时优先）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
)

// ModelInfo 模型列表条目（最小集合：只取 id，透传剩余原始字段供面板展示）。
type ModelInfo struct {
	ID      string `json:"id"`
	RawJSON json.RawMessage
}

// staticModels 真实模型目录（来源三路交叉，2026-09-27 实测全部 200）：
//  1. Marvis 桌面客户端 1.0.0.10371 解密配置 `models:` 注册表；
//  2. 上游字典探测收敛（136 个候选名，响应 4130001=模型不存在）；
//  3. Artificial Analysis 当前模型表做字段级匹配（响应 model 字段核对后端解析名）。
//
// 上游没有 /v1/models 端点，此目录即权威；upstream.models_path 配置了可用
// 端点时仍优先动态拉取。
var staticModels = []ModelInfo{
	{ID: "deepseek-v4-pro-external"},
	{ID: "deepseek-v3.2"},
	{ID: "main-auto"},
	{ID: "app-auto"},
	{ID: "file-auto"},
	{ID: "computer-auto"},
	{ID: "browser-auto"},
	{ID: "search-auto"},
	{ID: "hy4-preview"},
	{ID: "hy3-preview-agent"},
	{ID: "hy3-preview-agent-real"},
	{ID: "hy3"},
	{ID: "kimi-k2.5"},
	{ID: "hunyuan-2.0-instruct-20251111"},
	{ID: "hunyuan-turbos-vision"},
	{ID: "venus-vision-llm"},
	{ID: "glm-5v-turbo"},
	{ID: "gpt-5.4"},
	{ID: "claude-sonnet-4-6"},
	{ID: "claude-opus-4-6"},
	{ID: "ark_deepseek-v4-flash-ga-260731"},
	{ID: "qwen-memory-compression-int8"},
	{ID: "observation-compress-model"},
	{ID: "grounder-model"},
}

// probeModel 探活用的最小对话模型：非思考、响应快、直接出 content。
const probeModel = "hunyuan-2.0-instruct-20251111"

// modelsCacheTTL 动态目录缓存时长；负缓存（失败后）短一半。
const (
	modelsCacheTTL    = time.Hour
	modelsNegCacheTTL = 5 * time.Minute
)

// ModelsLive 不读模型目录缓存，直接探测上游可达性。
// 配置了 models_path → 请求上游模型列表；未配置（默认：上游无该端点）→
// 发一次 1-token 最小对话（唯一已鉴权的轻量探针）。
func (c *Client) ModelsLive(ctx context.Context, a *account.Auth) ([]ModelInfo, error) {
	if c.ModelsPath == "" {
		if err := c.ProbeChat(ctx, a); err != nil {
			return nil, err
		}
		return StaticModels(), nil
	}
	return c.fetchModels(ctx, a)
}

// ProbeChat 发一次 max_tokens=1 的最小对话验证账号与链路可用。
// 上游会给探针注入自身 system prompt（实测 prompt≈3k token），故探针
// 只适合手动/低频触发（探活按钮、keepalive），不做周期默认开启。
func (c *Client) ProbeChat(ctx context.Context, a *account.Auth) error {
	body, err := json.Marshal(map[string]any{
		"model":      probeModel,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"stream":     false,
		"max_tokens": 1,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatEndpoint(a), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c.applyCommonHeaders(req, a)
	c.applyChatHeaders(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read probe body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateStr(string(raw), 200)}
	}
	return nil
}

// Models 拉取模型目录：models_path 未配置（默认）→ 直接返回真实静态目录；
// 配置了则缓存优先（1h），未命中时动态拉取，失败回退静态目录并记 5min
// 负缓存防打爆。容忍两种响应形态：标准 OpenAI {data:[...]} 与顶层数组。
func (c *Client) Models(ctx context.Context, a *account.Auth) ([]ModelInfo, error) {
	if c.ModelsPath == "" {
		return StaticModels(), nil
	}
	c.modelsMu.RLock()
	now := time.Now()
	if len(c.modelsCache) > 0 && now.Sub(c.modelsCachedAt) < modelsCacheTTL {
		out := c.modelsCache
		c.modelsMu.RUnlock()
		return out, nil
	}
	negOK := !c.modelsFailedAt.IsZero() && now.Sub(c.modelsFailedAt) < modelsNegCacheTTL
	c.modelsMu.RUnlock()
	if negOK {
		// 负缓存期内直接回退静态目录（不报错——调用方拿到可用目录）。
		return append([]ModelInfo(nil), staticModels...), nil
	}

	items, err := c.fetchModels(ctx, a)
	c.modelsMu.Lock()
	if err != nil {
		c.modelsFailedAt = time.Now()
		c.modelsMu.Unlock()
		// 降级：静态目录兜底，错误一并返回（调用方按需记日志）。
		return append([]ModelInfo(nil), staticModels...), err
	}
	if len(items) == 0 {
		c.modelsFailedAt = time.Now()
		c.modelsMu.Unlock()
		return append([]ModelInfo(nil), staticModels...), errors.New("models 响应为空")
	}
	c.modelsCache = items
	c.modelsCachedAt = time.Now()
	c.modelsFailedAt = time.Time{}
	c.modelsMu.Unlock()
	return items, nil
}

// fetchModels 单次动态拉取（GET ModelsPath，Bearer 鉴权）。
func (c *Client) fetchModels(ctx context.Context, a *account.Auth) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.modelsEndpoint(a), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.applyCommonHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncateStr(string(raw), 200)}
	}
	return parseModels(raw)
}

// parseModels 解析模型列表 JSON（data 数组形态 / 顶层数组形态双兼容）。
func parseModels(raw []byte) ([]ModelInfo, error) {
	var top struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &top); err == nil && top.Data != nil {
		return toModelInfos(top.Data)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		return toModelInfos(arr)
	}
	return nil, errors.New("models 响应既不是 {data:[...]} 也不是数组")
}

func toModelInfos(items []json.RawMessage) ([]ModelInfo, error) {
	out := make([]ModelInfo, 0, len(items))
	for _, it := range items {
		var mi struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(it, &mi); err != nil || mi.ID == "" {
			continue // 无 id 的条目跳过（不阻塞整表）
		}
		out = append(out, ModelInfo{ID: mi.ID, RawJSON: it})
	}
	if len(out) == 0 {
		return nil, errors.New("models 响应为空")
	}
	return out, nil
}

// StaticModels 返回真实模型目录的副本（/v1/models 的兜底形态；上游无
// 模型列表端点时即权威目录）。
func StaticModels() []ModelInfo {
	return append([]ModelInfo(nil), staticModels...)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
