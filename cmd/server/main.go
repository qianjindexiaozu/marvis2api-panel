// main.go marvis2api-panel 入口：加载配置、装配状态机/上游/调度/面板，
// 起 HTTP 服务并做优雅停机。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/apikey"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/gateway"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/kernel"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/livecfg"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/panel"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/prepare"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/scheduler"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/usage"
)

// appVersion 网关版本（透出到 /panel/api/overview 与 /status）。
var appVersion = "dev"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
func usagePathFor(stateFile string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return "usage.json"
	}
	return filepath.Join(dir, "usage.json")
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	preparePath := os.Getenv("MV2A_PREPARE_FILE")
	if preparePath == "" {
		preparePath = prepare.DefaultPath
	}
	prepared, err := prepare.Load(preparePath)
	if err != nil {
		log.Fatalf("load prepare: %v；请先运行 bash scripts/prepare.sh，或设置 MV2A_PREPARE_FILE", err)
	}
	log.Printf("[prepare] 已加载签名配置和设备号")

	cfg, err := Load(*cfgPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含默认 api_key）再加载。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			} else {
				log.Printf("config %s 自动生成失败（%v），使用默认配置", *cfgPath, werr)
				cfg, err = ParseConfig(nil)
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// 凭证：单文件 auth.json（0600）。
	authFile := account.NewFile(cfg.AuthFile)
	st := state.New(cfg.StateFile)
	defer st.Close()
	st.SetBreaker(cfg.Cooldown.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxDur)
	st.SetMaxInFlight(cfg.MaxInFlight)
	st.SetSoftRateMax(cfg.SoftRateMaxDur)
	st.SetNotFoundCooldown(cfg.NotFoundDur)
	st.SetAuthDeadCooldown(cfg.AuthDeadDur)
	a, loadErr := authFile.Load()
	if loadErr != nil {
		log.Printf("WARN: [auth] %v", loadErr)
	}
	if a != nil {
		st.SetAuth(a)
		log.Printf("loaded credential from %s（%s）", authFile.Path(), a.Label())
	} else {
		log.Printf("no credential yet: %s（请在面板「账号」里扫码或手工添加）", authFile.Path())
	}

	// 凭证文件热加载：手工放文件 / 外部生成凭证免重启生效。
	watcher := state.StartWatch(st, authFile, 0)
	defer watcher.Stop()

	// 上游客户端：chat 无总超时（长流式），首字节/流中空闲上限单独设。
	up := upstream.New()
	up.AccessKey = prepared.AccessKey
	up.DeviceQIMEI = prepared.QIMEI36
	up.HTTP.Timeout = cfg.TimeoutDur
	up.SetHeaderTimeout(cfg.HeaderTimeoutDur)
	up.IdleTimeout = cfg.IdleTimeoutDur
	up.BaseURL = cfg.Upstream.BaseURL
	up.ChatPath = cfg.Upstream.ChatPath
	up.ModelsPath = cfg.Upstream.ModelsPath
	up.QuotaPath = cfg.Upstream.QuotaPath
	up.RefreshPath = cfg.Upstream.RefreshPath
	up.UserAgent = cfg.Upstream.UserAgent
	up.SanitizeFingerprints = cfg.sanitizeEnabled()
	up.SetExtraFingerprints(cfg.Features.ExtraFingerprints)

	// 多账号：目录里一份凭证对应一份官方内核。旧 auth.json 会导入一次。
	store := account.NewStore(filepath.Join(filepath.Dir(cfg.AuthFile), "accounts"))
	if a != nil {
		if err := store.Import(a); err != nil {
			log.Printf("WARN: [accounts] 导入现有凭证失败: %v", err)
		}
	}
	accounts, listErr := store.List()
	if listErr != nil {
		log.Printf("WARN: [accounts] %v", listErr)
	}
	var active *account.Auth
	if id := store.Active(); id != "" {
		active, _ = store.Load(id)
	}
	if active == nil && len(accounts) > 0 {
		active = accounts[0]
		_ = store.SetActive(active.ID())
	}
	if active != nil {
		st.SetAuth(active)
	}
	kernels := kernel.New(filepath.Join(filepath.Dir(cfg.AuthFile), "kernels"))
	defer kernels.Close()
	kernels.SetRefresher(func(ctx context.Context, acc *account.Auth) error {
		if err := up.RefreshToken(ctx, acc); err != nil {
			return err
		}
		_ = authFile.Save(acc)
		return store.Save(acc)
	})
	if active != nil {
		kernels.SetActive(active.ID())
	}
	if kernel.Installed() {
		log.Printf("[kernel] 本机已安装 Marvis，账号会各自启动一份内核")
		for _, acc := range accounts {
			kernels.Ensure(acc)
		}
	} else {
		log.Printf("[kernel] 本机没有 Marvis，聊天仍走上游 HTTP")
	}

	// livecfg：软冷却基数的读写点。网关密钥只认密钥库，不再使用配置里的 api_key。
	live := livecfg.New(livecfg.Snapshot{
		SoftCooldown: cfg.SoftRateDur,
	})
	keyFile := filepath.Join(filepath.Dir(cfg.AuthFile), "api_keys.json")
	keys := apikey.NewStore(keyFile)
	keys.Delete("legacy")
	passwords := panel.NewPassFile(filepath.Join(filepath.Dir(cfg.AuthFile), "panel_password"))
	if passwords.Get() == "" {
		if err := passwords.Set(panel.DefaultPassword); err != nil {
			log.Printf("[panel] 写入默认登录密码失败: %v", err)
		}
	}
	dropConfigAPIKey(*cfgPath)

	// 用量记录器：与 state 文件同目录。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s", usagePath)

	// 后台调度：配额刷新 / 保活 / 探测。
	sched := scheduler.New(scheduler.Config{
		State:                st,
		Upstream:             up,
		File:                 authFile,
		Accounts:             store,
		QuotaRefreshMinutes:  quotaRefreshMinutes(cfg),
		KeepaliveHours:       keepaliveHours(cfg),
		ProbeIntervalSeconds: probeInterval(cfg),
		Timeout:              cfg.TimeoutDur,
	})

	// 网关 handler（先于面板构建：面板「聊天测试/请求统计」经同进程代理访问网关）。
	gh := gateway.New(gateway.Config{
		State:         st,
		Upstream:      up,
		Live:          live,
		APIKey:        cfg.APIKey,
		Usage:         rec,
		Version:       appVersion,
		SoftCooldown:  cfg.SoftRateDur,
		ModelsTimeout: cfg.ModelsTimeoutDur,
		Seq:           ghSeq,
		Kernels:       kernels,
		Accounts:      store,
		Keys:          keys,
	})

	// 管理面板：日志环形缓冲镜像 stderr。
	pn := panel.New(panel.Config{
		State:        st,
		File:         authFile,
		Usage:        rec,
		APIKey:       cfg.APIKey,
		Version:      appVersion,
		Live:         live,
		Upstream:     up,
		Sched:        sched,
		ProbeTimeout: cfg.ModelsTimeoutDur,
		ConfigPath:   *cfgPath,
		Metrics:      gh.Metrics(),
		Gateway:      gh,
		Accounts:     store,
		Kernels:      kernels,
		Passwords:    passwords,
		Keys:         keys,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, st, up)
		},
	})
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))

	mux := http.NewServeMux()
	mux.Handle("/v1/", gh)
	mux.Handle("/healthz", gh)
	mux.Handle("/status", gh)
	mux.Handle("/panel", pn) // /panel → 面板（panel 内部处理 /panel 前缀）
	mux.Handle("/panel/", pn)
	mux.Handle("/admin/", pn) // 家族约定的管理别名（panel 内部路由）

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		sched.Run(ctx)
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		// 不设全局 WriteTimeout：长流式生成合法可达数分钟，全局 WriteTimeout 会误杀在途 SSE。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		st.Flush() // 信号触发：先落盘再优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("marvis2api listening on %s，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// ghSeq 网关请求序号发生器（chat 日志表格行 #NNN 段）。
var ghSeq = gatewaySeq()

// gatewaySeq 返回从 1 开始的单调序号函数。并发请求各自取号，用原子加避免重复。
func gatewaySeq() func() int64 {
	var n atomic.Int64
	return func() int64 {
		return n.Add(1)
	}
}

// keepaliveHours 保活整点（显式关闭时为空）。
func keepaliveHours(c *Config) []int {
	if !c.keepaliveEnabled() {
		return nil
	}
	return c.Schedule.KeepaliveHours
}

// quotaRefreshMinutes 配额刷新周期分钟数（显式关闭时 0）。
func quotaRefreshMinutes(c *Config) int {
	if !c.quotaRefreshEnabled() {
		return 0
	}
	return c.Schedule.QuotaRefreshMinutes
}

// probeInterval 探测周期秒数（显式关闭时 0）。
func probeInterval(c *Config) int {
	if !c.probeEnabled() {
		return 0
	}
	return c.Schedule.ProbeIntervalSec
}

// panelListenPath 从 listen 地址提取 ":port" 形式（启动日志拼面板 URL）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围：api_key / cooldown.soft_rate（livecfg）+ 熔断与并发参数 + 脱敏开关。
// 需重启：listen / auth_file / state_file / upstream.* / schedule 时点。
// 落盘用"先写 tmp 再 rename"原子替换；深合并保留用户手写的未知键。
func saveConfig(raw []byte, path string, live *livecfg.Holder, st *state.State, up *upstream.Client) ([]string, error) {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("解析提交的配置: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)
	delete(merged, "api_key")
	newCfg, err := ParseConfig(mustJSON(merged))
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := atomicWriteFile(path, out); err != nil {
		return nil, err
	}

	// 热应用。
	live.Store(livecfg.Snapshot{
		APIKey:       newCfg.APIKey,
		SoftCooldown: newCfg.SoftRateDur,
	})
	st.SetBreaker(newCfg.Cooldown.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxDur)
	st.SetMaxInFlight(newCfg.MaxInFlight)
	st.SetSoftRateMax(newCfg.SoftRateMaxDur)
	st.SetNotFoundCooldown(newCfg.NotFoundDur)
	st.SetAuthDeadCooldown(newCfg.AuthDeadDur)
	up.SanitizeFingerprints = newCfg.sanitizeEnabled()
	up.SetExtraFingerprints(newCfg.Features.ExtraFingerprints)

	return []string{
		"listen", "auth_file", "state_file",
		"upstream.base_url", "upstream.chat_path", "upstream.models_path",
		"upstream.quota_path", "upstream.refresh_path",
		"upstream.timeout_seconds", "upstream.header_timeout_seconds",
		"upstream.idle_timeout_seconds", "upstream.models_timeout_seconds",
		"schedule.quota_refresh_minutes", "schedule.keepalive_hours",
	}, nil
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 面板表单只提交它管理的键，未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mustJSON 序列化（失败返回 "{}"，仅校验路径，不可达失败）。
func mustJSON(m map[string]any) []byte {
	raw, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// dropConfigAPIKey 去掉配置文件里的 api_key。登录密码和网关密钥都不再用它。
func dropConfigAPIKey(path string) {
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if _, ok := m["api_key"]; !ok {
		return
	}
	delete(m, "api_key")
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := atomicWriteFile(path, append(out, '\n')); err != nil {
		log.Printf("[config] 移除 api_key 失败: %v", err)
	}
}

// atomicWriteFile tmp + rename 原子替换（0600）。
// 单文件 Docker bind mount 无法 rename 覆盖挂载点（Linux EBUSY）：
// 回退为原地写入（非原子，功能不受影响，UI 已有说明）。
func atomicWriteFile(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		if !errors.Is(err, syscall.EBUSY) {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace config: %w", err)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(raw)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}
	return nil
}
