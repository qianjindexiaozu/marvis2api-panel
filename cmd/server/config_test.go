// config_test.go 配置默认值、JSON 解析、时长换算、环境变量覆盖与默认配置落盘的单元测试。
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseConfigDefaults 空配置 → 全部推荐默认值（单账号形态 auth_file/state_file）。
func TestParseConfigDefaults(t *testing.T) {
	c, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("空配置应可用: %v", err)
	}
	if c.Listen != ":18620" {
		t.Fatalf("默认 listen 应 :18620，得到 %s", c.Listen)
	}
	if c.AuthFile != "./data/auth.json" {
		t.Fatalf("默认 auth_file 应 ./data/auth.json，得到 %s", c.AuthFile)
	}
	if c.StateFile != "./data/state.json" {
		t.Fatalf("默认 state_file 应 ./data/state.json，得到 %s", c.StateFile)
	}
	if c.Upstream.BaseURL != "https://yybadaccess.3g.qq.com" {
		t.Fatalf("默认上游根地址不符: %s", c.Upstream.BaseURL)
	}
	if c.Upstream.ChatPath != "/v1/chat/completions" {
		t.Fatalf("默认 chat 路径不符: %s", c.Upstream.ChatPath)
	}
	if c.Upstream.ModelsPath != "" {
		t.Fatalf("models_path 默认应为空（空 = 内置真实模型目录），得到 %q", c.Upstream.ModelsPath)
	}
	// 超时默认：120/120/300/15 秒。
	if c.TimeoutDur != 120*time.Second {
		t.Fatalf("默认总超时应 120s，得到 %v", c.TimeoutDur)
	}
	if c.HeaderTimeoutDur != 120*time.Second {
		t.Fatalf("默认首字节超时应 120s，得到 %v", c.HeaderTimeoutDur)
	}
	if c.IdleTimeoutDur != 300*time.Second {
		t.Fatalf("默认流空闲超时应 300s，得到 %v", c.IdleTimeoutDur)
	}
	if c.ModelsTimeoutDur != 15*time.Second {
		t.Fatalf("默认模型列表超时应 15s，得到 %v", c.ModelsTimeoutDur)
	}
	// 冷却默认：600s / 2h / 60s / 24h。
	if c.SoftRateDur != 600*time.Second || c.SoftRateMaxDur != 2*time.Hour {
		t.Fatalf("默认软冷却不符: %v %v", c.SoftRateDur, c.SoftRateMaxDur)
	}
	if c.NotFoundDur != 60*time.Second {
		t.Fatalf("默认 404 冷却应 60s，得到 %v", c.NotFoundDur)
	}
	if c.AuthDeadDur != 24*time.Hour {
		t.Fatalf("默认凭证死亡冷却应 24h，得到 %v", c.AuthDeadDur)
	}
	// 熔断默认：阈值 3 / 30m / 6h。
	if c.Cooldown.BreakerThreshold != 3 {
		t.Fatalf("默认熔断阈值应 3，得到 %d", c.Cooldown.BreakerThreshold)
	}
	if c.BreakerCooldownDur != 30*time.Minute || c.BreakerCooldownMaxDur != 6*time.Hour {
		t.Fatalf("默认熔断时长不符: %v %v", c.BreakerCooldownDur, c.BreakerCooldownMaxDur)
	}
	// 指针布尔：未写 = 开；探测例外（默认关）。
	if !c.sanitizeEnabled() || !c.quotaRefreshEnabled() || !c.keepaliveEnabled() {
		t.Fatal("未配置的开关应默认开启")
	}
	if c.probeEnabled() {
		t.Fatal("探测未配置应默认关闭")
	}
}

// TestParseConfigValidJSON 合法 JSON 的解析与时长换算（秒数字段 + 时长字符串）。
func TestParseConfigValidJSON(t *testing.T) {
	raw := `{
		"listen": ":20000",
		"api_key": "k1",
		"auth_file": "/tmp/auth.json",
		"state_file": "/tmp/state.json",
		"max_in_flight": 4,
		"upstream": {
			"base_url": "https://example.com",
			"chat_path": "/chat",
			"quota_path": "/api/quota",
			"timeout_seconds": 30,
			"models_timeout_seconds": 5,
			"user_agent": "ua/2.0"
		},
		"cooldown": {
			"soft_rate": "90s",
			"soft_rate_max": "45m",
			"not_found": "2m",
			"auth_dead": "1h",
			"breaker_threshold": 5,
			"breaker_cooldown": "10m",
			"breaker_cooldown_max": "90m"
		},
		"schedule": {"keepalive_hours": [8, 22]}
	}`
	c, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":20000" || c.APIKey != "k1" || c.AuthFile != "/tmp/auth.json" || c.StateFile != "/tmp/state.json" {
		t.Fatalf("顶层解析不符: %s %s %s %s", c.Listen, c.APIKey, c.AuthFile, c.StateFile)
	}
	if c.MaxInFlight != 4 {
		t.Fatalf("max_in_flight 应为 4，得到 %d", c.MaxInFlight)
	}
	if c.Upstream.BaseURL != "https://example.com" || c.Upstream.ChatPath != "/chat" ||
		c.Upstream.QuotaPath != "/api/quota" || c.Upstream.UserAgent != "ua/2.0" {
		t.Fatalf("上游解析不符: %+v", c.Upstream)
	}
	// 秒数 → Duration。
	if c.TimeoutDur != 30*time.Second || c.ModelsTimeoutDur != 5*time.Second {
		t.Fatalf("秒数换算不符: %v %v", c.TimeoutDur, c.ModelsTimeoutDur)
	}
	// 时长字符串 → Duration。
	if c.SoftRateDur != 90*time.Second || c.SoftRateMaxDur != 45*time.Minute ||
		c.NotFoundDur != 2*time.Minute || c.AuthDeadDur != time.Hour {
		t.Fatalf("冷却时长换算不符: %v %v %v %v", c.SoftRateDur, c.SoftRateMaxDur, c.NotFoundDur, c.AuthDeadDur)
	}
	if c.Cooldown.BreakerThreshold != 5 || c.BreakerCooldownDur != 10*time.Minute ||
		c.BreakerCooldownMaxDur != 90*time.Minute {
		t.Fatalf("熔断解析不符: %d %v %v",
			c.Cooldown.BreakerThreshold, c.BreakerCooldownDur, c.BreakerCooldownMaxDur)
	}
	if len(c.Schedule.KeepaliveHours) != 2 || c.Schedule.KeepaliveHours[0] != 8 || c.Schedule.KeepaliveHours[1] != 22 {
		t.Fatalf("保活整点解析不符: %v", c.Schedule.KeepaliveHours)
	}
}

// TestParseDur parseDur 各形态：空串默认、纯数字按秒、带单位、非法报错。
func TestParseDur(t *testing.T) {
	if d, err := parseDur("", 7*time.Second); err != nil || d != 7*time.Second {
		t.Fatalf("空串应返回默认值: %v %v", d, err)
	}
	if d, err := parseDur("120", 0); err != nil || d != 120*time.Second {
		t.Fatalf("纯数字应按秒解析: %v %v", d, err)
	}
	units := map[string]time.Duration{"30s": 30 * time.Second, "5m": 5 * time.Minute, "2h": 2 * time.Hour}
	for s, want := range units {
		d, err := parseDur(s, 0)
		if err != nil || d != want {
			t.Fatalf("时长 %q 应为 %v: %v %v", s, want, d, err)
		}
	}
	for _, bad := range []string{"abc", "1x", "-5", "-1m"} {
		if _, err := parseDur(bad, 0); err == nil {
			t.Fatalf("非法时长 %q 应报错", bad)
		}
	}
}

// TestParseConfigKeepaliveHoursRange 保活整点限定 0-23，越界启动即报错。
func TestParseConfigKeepaliveHoursRange(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"schedule":{"keepalive_hours":[25]}}`)); err == nil {
		t.Fatal("小时值 25 应报错")
	}
	if _, err := ParseConfig([]byte(`{"schedule":{"keepalive_hours":[-1]}}`)); err == nil {
		t.Fatal("小时值 -1 应报错")
	}
	if _, err := ParseConfig([]byte(`{"schedule":{"keepalive_hours":[0,23]}}`)); err != nil {
		t.Fatalf("边界值 0/23 应合法: %v", err)
	}
}

// TestParseConfigExplicitFalse 指针布尔显式取值应生效（区别于未写的默认）。
func TestParseConfigExplicitFalse(t *testing.T) {
	c, err := ParseConfig([]byte(`{"features":{"sanitize_fingerprints":false},"schedule":{"probe_enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.sanitizeEnabled() {
		t.Fatal("脱敏显式 false 应生效")
	}
	if !c.probeEnabled() {
		t.Fatal("探测显式 true 应生效")
	}
}

// TestApplyEnv 六个 MV2A_* 环境变量覆盖（非空才生效）。
func TestApplyEnv(t *testing.T) {
	t.Setenv("MV2A_LISTEN", ":19999")
	t.Setenv("MV2A_API_KEY", "envkey")
	t.Setenv("MV2A_AUTH_FILE", "/env/auth.json")
	t.Setenv("MV2A_STATE_FILE", "/env/state.json")
	t.Setenv("MV2A_BASE_URL", "https://env.example.com")
	t.Setenv("MV2A_USER_AGENT", "env-agent/1.0")
	c, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":19999" || c.APIKey != "envkey" {
		t.Fatalf("listen/api_key 覆盖未生效: %s %s", c.Listen, c.APIKey)
	}
	if c.AuthFile != "/env/auth.json" || c.StateFile != "/env/state.json" {
		t.Fatalf("凭证/状态文件覆盖未生效: %s %s", c.AuthFile, c.StateFile)
	}
	if c.Upstream.BaseURL != "https://env.example.com" || c.Upstream.UserAgent != "env-agent/1.0" {
		t.Fatalf("上游覆盖未生效: %s %s", c.Upstream.BaseURL, c.Upstream.UserAgent)
	}

	// 空值不覆盖：listen/api_key 回落默认。
	t.Setenv("MV2A_LISTEN", "")
	t.Setenv("MV2A_API_KEY", "")
	c2, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Listen != ":18620" || c2.APIKey != "" {
		t.Fatalf("空环境变量不应覆盖: %s %q", c2.Listen, c2.APIKey)
	}
}

// TestWriteDefaultAndLoad WriteDefault 落盘默认配置且 api_key=marvis、可再 Load。
func TestWriteDefaultAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	key, err := WriteDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	if key != "marvis" {
		t.Fatalf("默认密钥应为 marvis，得到 %q", key)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != key {
		t.Fatalf("落盘密钥应可回读: %s vs %s", c.APIKey, key)
	}
	if c.Listen != ":18620" || c.AuthFile != "./data/auth.json" || c.StateFile != "./data/state.json" {
		t.Fatalf("默认配置回读不符: %s %s %s", c.Listen, c.AuthFile, c.StateFile)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("配置文件应 0600，得到 %v", fi.Mode().Perm())
	}
}

// TestMergeConfigMaps 深合并语义：兄弟键保留、嵌套合并、新键写入。
func TestMergeConfigMaps(t *testing.T) {
	cur := map[string]any{
		"listen":   ":1",
		"upstream": map[string]any{"base_url": "https://a.com", "keep_me": true},
		"custom":   1,
	}
	in := map[string]any{
		"upstream": map[string]any{"base_url": "https://b.com"},
		"new":      "x",
	}
	out := mergeConfigMaps(cur, in)
	if out["listen"] != ":1" || out["custom"] != 1 {
		t.Fatal("兄弟键应保留")
	}
	um := out["upstream"].(map[string]any)
	if um["base_url"] != "https://b.com" || um["keep_me"] != true {
		t.Fatalf("深合并语义不符: %v", um)
	}
	if out["new"] != "x" {
		t.Fatal("新键应写入")
	}
}
