// Config 配置定义、默认值、规范化与环境变量覆盖。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 网关配置（config.json 形态见 config.example.json）。
//
// auth_file 用于当前账号的兼容快照；多账号凭证保存在同目录的 accounts/。
type Config struct {
	Listen    string      `json:"listen"`
	APIKey    string      `json:"api_key"`
	AuthFile  string      `json:"auth_file"`
	StateFile string      `json:"state_file"`
	Upstream  UpstreamCfg `json:"upstream"`
	Cooldown  CooldownCfg `json:"cooldown"`
	Schedule  ScheduleCfg `json:"schedule"`
	Features  FeaturesCfg `json:"features"`

	// MaxInFlight 网关最大并发在途请求；0 = 不限。
	MaxInFlight int `json:"max_in_flight"`

	// normalize 后的派生值（不序列化）。
	TimeoutDur            time.Duration `json:"-"`
	HeaderTimeoutDur      time.Duration `json:"-"`
	IdleTimeoutDur        time.Duration `json:"-"`
	ModelsTimeoutDur      time.Duration `json:"-"`
	SoftRateDur           time.Duration `json:"-"`
	SoftRateMaxDur        time.Duration `json:"-"`
	NotFoundDur           time.Duration `json:"-"`
	AuthDeadDur           time.Duration `json:"-"`
	BreakerCooldownDur    time.Duration `json:"-"`
	BreakerCooldownMaxDur time.Duration `json:"-"`
}

// UpstreamCfg 上游 HTTP 客户端参数。端点常量可整体覆盖：上游协议演进时
// 只改配置，不动代码。
type UpstreamCfg struct {
	BaseURL    string `json:"base_url"`    // 上游根地址（凭证可再覆盖）
	ChatPath   string `json:"chat_path"`   // chat 端点路径
	ModelsPath string `json:"models_path"` // 模型列表路径
	// QuotaPath 自定义配额查询路径；空时查询官方钱包接口。
	QuotaPath string `json:"quota_path"`
	// RefreshPath token 刷新路径；空时使用官方默认路径。
	RefreshPath string `json:"refresh_path"`

	TimeoutSeconds       int    `json:"timeout_seconds"`        // 短 RPC 总超时
	HeaderTimeoutSeconds int    `json:"header_timeout_seconds"` // chat SSE 首字节前上限
	IdleTimeoutSeconds   int    `json:"idle_timeout_seconds"`   // chat SSE 流中空闲上限
	ModelsTimeoutSeconds int    `json:"models_timeout_seconds"` // 模型列表单请求超时
	UserAgent            string `json:"user_agent"`
}

// CooldownCfg 冷却与熔断参数。
type CooldownCfg struct {
	SoftRate    string `json:"soft_rate"`     // 429 软冷却基数
	SoftRateMax string `json:"soft_rate_max"` // 指数退避封顶
	NotFound    string `json:"not_found"`     // 404 固定短冷却
	AuthDead    string `json:"auth_dead"`     // 401/403 硬冷却时长

	BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败熔断阈值
	BreakerCooldown    string `json:"breaker_cooldown"`     // 首次熔断时长
	BreakerCooldownMax string `json:"breaker_cooldown_max"` // 熔断退避封顶
}

// ScheduleCfg 后台任务参数。
type ScheduleCfg struct {
	QuotaRefreshMinutes int   `json:"quota_refresh_minutes"` // 配额刷新周期（分钟；0=关）
	QuotaRefreshEnabled *bool `json:"quota_refresh_enabled"` // 指针区分"未写"（默认开）与"显式 false"
	KeepaliveHours      []int `json:"keepalive_hours"`       // 保活整点；空 = 关闭
	KeepaliveEnabled    *bool `json:"keepalive_enabled"`
	ProbeIntervalSec    int   `json:"probe_interval_seconds"` // 探测周期（秒；0=关）
	ProbeEnabled        *bool `json:"probe_enabled"`
}

// FeaturesCfg 功能开关。
type FeaturesCfg struct {
	// SanitizeFingerprints 出站指纹脱敏（默认开；指针区分未写）。
	SanitizeFingerprints *bool `json:"sanitize_fingerprints"`
	// ExtraFingerprints 额外脱敏词（默认词表之外追加）。
	ExtraFingerprints []string `json:"extra_fingerprints"`
}

// sanitizeEnabled 脱敏开关：未配置视为开。
func (c *Config) sanitizeEnabled() bool {
	return c.Features.SanitizeFingerprints == nil || *c.Features.SanitizeFingerprints
}

// boolOrDefault 指针布尔缺省回落。
func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// quotaRefreshEnabled 配额刷新开关。
func (c *Config) quotaRefreshEnabled() bool {
	return boolOrDefault(c.Schedule.QuotaRefreshEnabled, true)
}

// keepaliveEnabled 保活开关。
func (c *Config) keepaliveEnabled() bool { return boolOrDefault(c.Schedule.KeepaliveEnabled, true) }

// probeEnabled 探测开关。
func (c *Config) probeEnabled() bool { return boolOrDefault(c.Schedule.ProbeEnabled, false) }

// ParseConfig 解析 + 默认值 + 规范化（时长字符串 → Duration）。
// 未知 JSON 键被忽略（面板保存时经 mergeConfigMaps 保留原键）。
func ParseConfig(raw []byte) (*Config, error) {
	c := &Config{}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		if err := dec.Decode(c); err != nil {
			return nil, fmt.Errorf("解析配置: %w", err)
		}
	}
	applyDefaults(c)
	applyEnv(c)
	var err error
	if c.TimeoutDur, err = normSeconds(c.Upstream.TimeoutSeconds, 120); err != nil {
		return nil, err
	}
	if c.HeaderTimeoutDur, err = normSeconds(c.Upstream.HeaderTimeoutSeconds, 120); err != nil {
		return nil, err
	}
	if c.IdleTimeoutDur, err = normSeconds(c.Upstream.IdleTimeoutSeconds, 300); err != nil {
		return nil, err
	}
	if c.ModelsTimeoutDur, err = normSeconds(c.Upstream.ModelsTimeoutSeconds, 15); err != nil {
		return nil, err
	}
	if c.SoftRateDur, err = parseDur(c.Cooldown.SoftRate, 600*time.Second); err != nil {
		return nil, err
	}
	if c.SoftRateMaxDur, err = parseDur(c.Cooldown.SoftRateMax, 2*time.Hour); err != nil {
		return nil, err
	}
	if c.NotFoundDur, err = parseDur(c.Cooldown.NotFound, 60*time.Second); err != nil {
		return nil, err
	}
	if c.AuthDeadDur, err = parseDur(c.Cooldown.AuthDead, 24*time.Hour); err != nil {
		return nil, err
	}
	if c.BreakerCooldownDur, err = parseDur(c.Cooldown.BreakerCooldown, 30*time.Minute); err != nil {
		return nil, err
	}
	if c.BreakerCooldownMaxDur, err = parseDur(c.Cooldown.BreakerCooldownMax, 6*time.Hour); err != nil {
		return nil, err
	}
	// 小时值合法性：0-23，非法启动即报错（fail fast，避免静默错排程）。
	for _, h := range c.Schedule.KeepaliveHours {
		if h < 0 || h > 23 {
			return nil, fmt.Errorf("schedule.keepalive_hours 含非法小时值 %d（0-23）", h)
		}
	}
	return c, nil
}

// Load 从文件读取配置；文件不存在返回 fs.ErrNotExist 包装错误
// （main 据此自动生成默认配置）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(raw)
}

// applyDefaults 缺省值填充（空字段 → 推荐默认）。
func applyDefaults(c *Config) {
	if c.Listen == "" {
		c.Listen = ":18620"
	}
	if c.AuthFile == "" {
		c.AuthFile = "./data/auth.json"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	if c.Upstream.BaseURL == "" {
		c.Upstream.BaseURL = "https://yybadaccess.3g.qq.com"
	}
	if c.Upstream.ChatPath == "" {
		c.Upstream.ChatPath = "/v1/chat/completions"
	}
	if c.Upstream.RefreshPath == "" {
		// 已实测打通的官方刷新端点（协议还原自客户端 H5 SDK）。
		c.Upstream.RefreshPath = "/marvis_client/marvis_refresh_token"
	}
	// ModelsPath 允许为空：上游没有 /v1/models 端点，空 = 用内置真实模型目录
	// （探活随之走 1-token 最小对话）。只有显式配置了可用端点才动态拉取。
	if c.Upstream.TimeoutSeconds == 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Upstream.HeaderTimeoutSeconds == 0 {
		c.Upstream.HeaderTimeoutSeconds = 120
	}
	if c.Upstream.IdleTimeoutSeconds == 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if c.Upstream.ModelsTimeoutSeconds == 0 {
		c.Upstream.ModelsTimeoutSeconds = 15
	}
	if c.Cooldown.SoftRate == "" {
		c.Cooldown.SoftRate = "600s"
	}
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.Cooldown.NotFound == "" {
		c.Cooldown.NotFound = "60s"
	}
	if c.Cooldown.AuthDead == "" {
		c.Cooldown.AuthDead = "24h"
	}
	if c.Cooldown.BreakerCooldown == "" {
		c.Cooldown.BreakerCooldown = "30m"
	}
	if c.Cooldown.BreakerCooldownMax == "" {
		c.Cooldown.BreakerCooldownMax = "6h"
	}
	if c.Cooldown.BreakerThreshold == 0 {
		c.Cooldown.BreakerThreshold = 3
	}
}

// applyEnv 环境变量覆盖（非空才覆盖；容器部署友好）。
func applyEnv(c *Config) {
	if v := os.Getenv("MV2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("MV2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("MV2A_AUTH_FILE"); v != "" {
		c.AuthFile = v
	}
	if v := os.Getenv("MV2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("MV2A_BASE_URL"); v != "" {
		c.Upstream.BaseURL = v
	}
	if v := os.Getenv("MV2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
}

// normSeconds 秒数配置 → Duration（非正 → 默认）。
func normSeconds(n, def int) (time.Duration, error) {
	if n <= 0 {
		return time.Duration(def) * time.Second, nil
	}
	return time.Duration(n) * time.Second, nil
}

// parseDur 时长字符串解析：支持 "30s"/"5m"/"2h" 与纯数字（按秒）。
// 空串返回默认；非法格式报错（配置错误启动即失败，避免静默错配置）。
func parseDur(s string, def time.Duration) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("非法时长 %q（不能为负）", s)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("非法时长 %q（示例：30s / 5m / 2h）", s)
	}
	return d, nil
}

// DefaultAPIKey 默认面板/网关密钥（用户指定）：与 workbuddy2api-gui 的
// 内置默认口令同思路，首启零配置可用；面板会显示"默认密码"横幅提醒修改。
const DefaultAPIKey = "marvis"

// WriteDefault 生成推荐配置。api_key 固定用默认值 marvis（登录页密码即它；
// 公网部署务必修改）。双击 exe / 裸跑 docker 即开。
func WriteDefault(path string) (string, error) {
	raw := fmt.Sprintf(`{
  "listen": ":18620",
  "api_key": %q,
  "auth_file": "./data/auth.json",
  "state_file": "./data/state.json"
}`, DefaultAPIKey)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		return "", err
	}
	return DefaultAPIKey, nil
}
