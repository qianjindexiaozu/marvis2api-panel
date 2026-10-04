// Package panel 内嵌式 Web 管理面板：账号、密钥、探活、配额、在线改配置、日志和用量。
//
// 前端随二进制嵌入，没有单独的前端构建。面板登录密码和调用 /v1 的 API Key 分开。
// 页面 HTML 可以匿名打开，/panel/api/* 需要登录。账号 token 不回传前端。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/apikey"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/httpauth"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/kernel"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/livecfg"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/metrics"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/oauth"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/scheduler"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/usage"
)

// DefaultPassword 默认面板密码（用户指定；与服务端 cmd/server.DefaultAPIKey 一致）。
// 放面板包是为 sessionInfo 的 using_default_password 判定；改默认值两处同步。
const DefaultPassword = "marvis"

// Config 面板依赖（main 装配注入）。
type Config struct {
	State *state.State
	File  *account.File
	Usage *usage.Recorder
	// APIKey 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先。
	APIKey  string
	Version string

	// Live 运行期可变配置（在线改 api_key 立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表。
	ConfigPath string
	LoadConfig func() (any, error)
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// Upstream 探测复用（立即探活 / 模型列表查询）。
	Upstream *upstream.Client
	// Sched 手动触发配额刷新（「刷新配额」按钮）；nil 时刷新接口 501。
	Sched *scheduler.Scheduler
	// ProbeTimeout 探活超时。
	ProbeTimeout time.Duration

	// Metrics 请求统计数据源（面板「请求统计」视图；nil 时回落 metrics.Global，
	// 与 gateway 的回落语义一致）。面板前端不直调 /v1/stats——凭证留在服务端。
	Metrics *metrics.Metrics
	// Accounts 多账号目录。nil 时仍只写单个 auth.json。
	Accounts *account.Store
	// Kernels 每账号一份官方内核。nil 或本机没有 Marvis 时不启动。
	Kernels *kernel.Pool

	// Passwords 面板登录密码。空文件时仍用 api_key，设置后与 API Key 分开。
	Passwords *PassFile
	// Keys 网关调用密钥。nil 时只认 api_key。
	Keys *apikey.Store

	// Gateway 网关 http.Handler（同进程注入）。面板「聊天测试」经
	// POST /panel/api/chat 代理：后端注入 Bearer api_key 后直调网关，
	// SSE 经由共享 ResponseWriter 自然透传；nil 时 501。
	Gateway http.Handler
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel", p) 与
// Handle("/panel/", p)，本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring
	sess    *SessionStore
	wxOAuth *oauth.Flow   // 微信扫码授权（无 App 登录）
	qqOAuth *oauth.QQFlow // QQ 扫码授权

	quotaMu sync.Mutex
	quotas  map[string]quotaSnap
	nameMu  sync.Mutex
	nameTry map[string]time.Time
	nameErr map[string]string
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 15 * time.Second
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		sess:    NewSessionStore(0),
		wxOAuth: oauth.NewFlow(),
		qqOAuth: oauth.NewQQFlow(),
	}
	if cfg.Upstream != nil {
		p.wxOAuth.AccessKey = cfg.Upstream.AccessKey
		p.qqOAuth.AccessKey = cfg.Upstream.AccessKey
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 输出进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel", p.index)
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)

	// 登录会话：登录页换发 HttpOnly 会话 Cookie。session 端点匿名可达（首屏探测）。
	p.mux.HandleFunc("POST /panel/api/login", p.login)
	p.mux.HandleFunc("POST /panel/api/logout", p.logout)
	p.mux.HandleFunc("GET /panel/api/session", p.sessionInfo)

	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usageHandler))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.modelsHandler))
	p.mux.HandleFunc("POST /panel/api/refresh", p.withAuth(p.refreshQuota))

	// 网关代理端点（前端不直调 /v1/*，凭证留服务端）。
	p.mux.HandleFunc("GET /panel/api/stats", p.withAuth(p.statsProxy))
	p.mux.HandleFunc("POST /panel/api/stats/reset", p.withAuth(p.statsResetProxy))
	p.mux.HandleFunc("POST /panel/api/chat", p.withAuth(p.chatProxy))

	// 凭证运维（单账号）。
	p.mux.HandleFunc("GET /panel/api/credential", p.withAuth(p.credentialGet))
	p.mux.HandleFunc("PUT /panel/api/credential", p.withAuth(p.credentialSave))
	p.mux.HandleFunc("DELETE /panel/api/credential", p.withAuth(p.credentialDelete))
	p.mux.HandleFunc("POST /panel/api/credential/probe", p.withAuth(p.credentialProbe))
	p.mux.HandleFunc("POST /panel/api/credential/quota", p.withAuth(p.credentialQuota))
	p.mux.HandleFunc("POST /panel/api/credential/refresh-token", p.withAuth(p.credentialRefreshToken))
	p.mux.HandleFunc("GET /panel/api/accounts", p.withAuth(p.accountsList))
	p.mux.HandleFunc("POST /panel/api/accounts/select", p.withAuth(p.accountSelect))
	p.mux.HandleFunc("POST /panel/api/accounts/dormant", p.withAuth(p.accountDormant))
	p.mux.HandleFunc("GET /panel/api/keys", p.withAuth(p.keysList))
	p.mux.HandleFunc("POST /panel/api/keys", p.withAuth(p.keysCreate))
	p.mux.HandleFunc("POST /panel/api/keys/check", p.withAuth(p.keysCheck))
	p.mux.HandleFunc("POST /panel/api/keys/reveal", p.withAuth(p.keysReveal))
	p.mux.HandleFunc("POST /panel/api/keys/disable", p.withAuth(p.keysDisable))
	p.mux.HandleFunc("DELETE /panel/api/keys", p.withAuth(p.keysDelete))
	p.mux.HandleFunc("POST /panel/api/password", p.withAuth(p.passwordSet))
	p.mux.HandleFunc("DELETE /panel/api/accounts", p.withAuth(p.accountDelete))
	p.mux.HandleFunc("POST /panel/api/accounts/probe", p.withAuth(p.accountProbe))
	p.mux.HandleFunc("POST /panel/api/accounts/quota", p.withAuth(p.accountQuota))
	p.mux.HandleFunc("POST /panel/api/accounts/refresh-token", p.withAuth(p.accountRefreshToken))
	p.mux.HandleFunc("POST /panel/api/login/wechat/start", p.withAuth(p.oauthWeChatStart))
	p.mux.HandleFunc("GET /panel/api/login/wechat/poll", p.withAuth(p.oauthWeChatPoll))
	p.mux.HandleFunc("POST /panel/api/login/qq/start", p.withAuth(p.oauthQQStart))
	p.mux.HandleFunc("GET /panel/api/login/qq/poll", p.withAuth(p.oauthQQPoll))
	p.mux.HandleFunc("POST /panel/api/credential/revive", p.withAuth(p.credentialRevive))

	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))

	// /admin/*：家族约定的管理别名（脚本兼容）。
	p.mux.HandleFunc("GET /admin/status", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /admin/credential", p.withAuth(p.credentialGet))
	p.mux.HandleFunc("POST /admin/refresh", p.withAuth(p.refreshQuota))
	p.mux.HandleFunc("POST /admin/revive", p.withAuth(p.credentialRevive))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 面板鉴权：会话 Cookie / Bearer 会话 token / Bearer api_key 三种
// 方式任一通过即放行（Bearer api_key 保留给 curl / 自动化脚本）。
// api_key 为空时放行（与主服务同语义：未启用鉴权）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.authorized(r) {
			writeErr(w, http.StatusUnauthorized,
				"unauthorized（未登录或会话过期；浏览器请从登录页进入，脚本可用 Bearer api_key）")
			return
		}
		next(w, r)
	}
}

// authorized 会话与密钥的统一判定。
func (p *Panel) authorized(r *http.Request) bool {
	if p.sess.Validate(sessionCookieValue(r)) || p.sess.Validate(bearerToken(r)) {
		return true
	}
	if p.cfg.Keys != nil && p.cfg.Keys.Accept(bearerToken(r)) {
		return true
	}
	key := p.apiKey()
	if key == "" {
		return p.loginPassword() == ""
	}
	return httpauth.VerifyBearer(r, key)
}

// login 登录：用面板密码，不用 API Key。
// 成功后颁发 HttpOnly 会话 Cookie（SameSite=Strict），浏览器后续请求自动携带。
func (p *Panel) login(w http.ResponseWriter, r *http.Request) {
	expect := p.loginPassword()
	if expect == "" {
		writeErr(w, http.StatusBadRequest, "未设置 api_key，面板处于免鉴权模式，无需登录")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	token, ok, msg := p.sess.Create(req.Password, expect, clientSource(r))
	if !ok {
		log.Printf("[panel] login failed source=%s", clientSource(r))
		writeErr(w, http.StatusUnauthorized, msg)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(p.sess.ttl.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// logout 注销当前会话（Cookie 与 Bearer token 均吊销）。
func (p *Panel) logout(w http.ResponseWriter, r *http.Request) {
	if t := sessionCookieValue(r); t != "" {
		p.sess.Revoke(t)
	}
	if t := bearerToken(r); t != "" {
		p.sess.Revoke(t)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sessionInfo 首屏探测：是否已登录 / 是否需要登录（匿名可达，不含敏感信息）。
func (p *Panel) sessionInfo(w http.ResponseWriter, r *http.Request) {
	pass := p.loginPassword()
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":          p.authorized(r),
		"auth_required":          pass != "",
		"using_default_password": pass == DefaultPassword,
		"version":                p.cfg.Version,
	})
}

// loginPassword 面板登录密码。单独设置过就用那份；否则沿用 api_key。两边都空则免登录。
func (p *Panel) loginPassword() string {
	if p.cfg.Passwords != nil {
		if s := p.cfg.Passwords.Get(); s != "" {
			return s
		}
	}
	return p.apiKey()
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	_, healthy := p.cfg.State.Summary()
	st := p.cfg.State.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"total":           1,
		"healthy":         healthy,
		"quota_remaining": st.QuotaRemaining,
		"quota_total":     st.QuotaTotal,
		"account":         st,
	})
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/probe/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// usageHandler 用量聚合。hours 查询参数控制统计窗口（默认 72，上限 1440）；
// hours=0 表示全部历史。
func (p *Panel) usageHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, ok := atoi(v); ok && n >= 0 {
			hours = n
		}
	}
	if hours > 1440 {
		hours = 1440
	}
	// 单账号：用量记录里的 account 就是账号标签本身，无需再做 ID→名称映射。
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot(hours, nil))
}

// modelsHandler 模型目录（动态拉取 + 静态兜底，见 upstream.Models）。
func (p *Panel) modelsHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.ProbeTimeout)
	defer cancel()
	items, err := p.cfg.Upstream.Models(ctx, nil)
	if err != nil && len(items) == 0 {
		writeErr(w, http.StatusBadGateway, "模型列表拉取失败: "+err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, mi := range items {
		entry := map[string]any{"id": mi.ID}
		if len(mi.RawJSON) > 0 {
			var extra map[string]any
			if json.Unmarshal(mi.RawJSON, &extra) == nil {
				for k, v := range extra {
					if k != "id" {
						entry[k] = v
					}
				}
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// refreshQuota 手动触发配额刷新（「刷新配额」按钮）。
func (p *Panel) refreshQuota(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Sched == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	// 响应返回后 r.Context() 会被取消。刷新在后台跑，必须脱离这次请求，
	// 否则上游调用会立刻被取消，按钮看起来像没生效。
	go p.cfg.Sched.RoundQuota(context.Background())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "配额刷新已触发（异步执行，稍后刷新查看结果）"})
}

// ---------------------------------------------------------------------------
// 网关代理（凭证不出服务端）
// ---------------------------------------------------------------------------

// metricsInst 生效的统计聚合器（注入优先，回落进程级全局，与 gateway 同语义）。
func (p *Panel) metricsInst() *metrics.Metrics {
	if p.cfg.Metrics != nil {
		return p.cfg.Metrics
	}
	return metrics.Global()
}

// statsProxy 请求统计：直接读同进程 metrics。
func (p *Panel) statsProxy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, p.metricsInst().SnapshotOf())
}

// statsResetProxy 清空请求统计累计。
func (p *Panel) statsResetProxy(w http.ResponseWriter, r *http.Request) {
	p.metricsInst().Reset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// chatProxy 聊天测试代理：克隆请求改写路径到 /v1/chat/completions，
// 注入 Bearer api_key 后直调同进程网关 handler。SSE 经共享 ResponseWriter
// 透传（面板侧 no-buffer），客户端中断经共享 context 传播到上游。
func (p *Panel) chatProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Gateway == nil {
		writeErr(w, http.StatusNotImplemented, "gateway not available")
		return
	}
	cr := r.Clone(r.Context())
	cr.URL.Path = "/v1/chat/completions"
	cr.URL.RawPath = ""
	cr.RequestURI = ""
	key := ""
	if p.cfg.Keys != nil {
		key = p.cfg.Keys.FirstEnabled()
		if key == "" {
			writeErr(w, http.StatusBadRequest, "还没有可用的 API Key，请先在密钥页生成")
			return
		}
	} else {
		key = p.apiKey()
	}
	if key != "" {
		cr.Header.Set("Authorization", "Bearer "+key)
	}
	p.cfg.Gateway.ServeHTTP(w, cr)
}

// ---------------------------------------------------------------------------
// 凭证运维（单账号）
// ---------------------------------------------------------------------------

// credentialGet 当前凭证的观测快照（不含 token）。
func (p *Panel) credentialGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"account":  p.cfg.State.Status(),
		"has_file": p.cfg.File.Exists(),
		"path":     p.cfg.File.Path(),
	})
}

// credentialSave 保存凭证：校验 → 原子落盘 auth.json → 立即进状态机（免重启）。
// token / refresh_token 传空 = 保留原值；换过 token 时自动解除硬冷却。
func (p *Panel) credentialSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		UID          string `json:"uid"`
		Guid         string `json:"guid"`
		BaseURL      string `json:"base_url"`
		LoginType    string `json:"login_type"`
		Note         string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求体: "+err.Error())
		return
	}
	cur := p.cfg.State.Auth()
	if cur == nil {
		cur = &account.Auth{}
	}
	a := cur.Clone()
	if req.Token != "" {
		a.Token = req.Token
	}
	if req.RefreshToken != "" {
		a.RefreshToken = req.RefreshToken
	}
	if req.Name != "" {
		a.Name = req.Name
	}
	if req.UID != "" {
		a.UID = req.UID
	}
	if req.Guid != "" {
		a.Guid = req.Guid
	}
	if req.BaseURL != "" {
		a.BaseURL = req.BaseURL
	}
	if req.LoginType != "" {
		a.LoginType = req.LoginType
	}
	a.Note = req.Note
	if err := a.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	changed, err := p.adopt(a)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "落盘失败: "+err.Error())
		return
	}
	log.Printf("[panel] credential saved label=%s（凭证%s）", a.Label(), map[bool]string{true: "已变更，硬冷却自动解除", false: "未变更"}[changed])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revived": changed, "account": p.cfg.State.Status()})
}

// credentialDelete 删除凭证文件并把状态机置为未配置。
func (p *Panel) credentialDelete(w http.ResponseWriter, r *http.Request) {
	id := ""
	if a := p.cfg.State.Auth(); a != nil {
		id = a.ID()
	}
	if id == "" && p.cfg.Accounts != nil {
		id = p.cfg.Accounts.Active()
	}
	if err := p.removeAccount(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除失败: "+err.Error())
		return
	}
	log.Printf("[panel] credential deleted")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// credentialProbe 立即探活（拉模型列表计延时，同步返回）。
func (p *Panel) credentialProbe(w http.ResponseWriter, r *http.Request) {
	a, ok := p.requireCredential(w)
	if !ok {
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.ProbeTimeout)
	defer cancel()
	started := time.Now()
	items, err := p.cfg.Upstream.ModelsLive(ctx, a)
	d := time.Since(started)
	nick := p.refreshName(ctx, a)
	if err != nil {
		p.cfg.State.SetProbeResult(false, d, "probe: "+publicErr(err))
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": publicErr(err), "latency_ms": d.Milliseconds(), "name": nick})
		return
	}
	p.cfg.State.SetProbeResult(true, d, "")
	models := make([]string, 0, len(items))
	for _, mi := range items {
		models = append(models, mi.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": d.Milliseconds(), "models": models, "name": nick})
}

// credentialQuota 立即查询配额（同步返回）。
func (p *Panel) credentialQuota(w http.ResponseWriter, r *http.Request) {
	a, ok := p.requireCredential(w)
	if !ok {
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.ProbeTimeout)
	defer cancel()
	q, err := p.cfg.Upstream.Quota(ctx, a)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	p.cfg.State.SetQuota(q.Remaining, q.Total)
	p.cfg.State.UnfreezeQuota(q.Remaining)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remaining": q.Remaining, "total": q.Total})
}

// credentialRefreshToken 用 refresh_token 向 upstream.refresh_path 换新 access token。
// 响应只带回成功或失败，不带回 token。
func (p *Panel) credentialRefreshToken(w http.ResponseWriter, r *http.Request) {
	a, ok := p.requireCredential(w)
	if !ok {
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client not available")
		return
	}
	if p.cfg.Upstream.RefreshPath == "" {
		writeErr(w, http.StatusNotImplemented, "未配置 upstream.refresh_path，无法刷新 token")
		return
	}
	if strings.TrimSpace(a.RefreshTokenValue()) == "" {
		writeErr(w, http.StatusBadRequest, "这个账号没有 refresh_token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := p.cfg.Upstream.RefreshToken(ctx, a); err != nil {
		log.Printf("[panel] refresh-token err=%v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": publicErr(err)})
		return
	}
	if err := p.cfg.File.Save(a); err != nil {
		writeErr(w, http.StatusInternalServerError, "新 token 落盘失败: "+err.Error())
		return
	}
	p.cfg.State.SetAuth(a)
	log.Printf("[panel] refresh-token ok")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// credentialRevive 手动复活：清冷却 + 熔断 + 失败计数（面板「解除冷却」按钮）。
func (p *Panel) credentialRevive(w http.ResponseWriter, r *http.Request) {
	p.cfg.State.Revive()
	log.Printf("[panel] revive（人工清除冷却/熔断）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": p.cfg.State.Status()})
}

// requireCredential 取当前凭证；未配置时直接写 404 并报告失败。
func (p *Panel) requireCredential(w http.ResponseWriter) (*account.Auth, bool) {
	a := p.cfg.State.Auth()
	if a == nil || a.TokenValue() == "" {
		writeErr(w, http.StatusNotFound, "还没有账号，请先在账号页扫码或手工添加")
		return nil, false
	}
	return a, true
}

// ---------------------------------------------------------------------------
// 配置读写
// ---------------------------------------------------------------------------

// getConfig 返回当前解析后的配置（结构化视图）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config loader not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": cfg})
}

// saveConfig 保存配置：校验 → 落盘 → 热应用，返回需重启的字段列表。
// api_key 变化时吊销全部旧会话，并给当前请求者换发新会话（它刚设置了新密钥），
// 其余已登录的浏览器一律重新登录——旧密钥立即失联。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config saver not available")
		return
	}
	raw := readAllLimit(r.Body, 4<<20)
	oldKey := p.apiKey()
	restart, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if newKey := p.apiKey(); newKey != oldKey && newKey != "" {
		p.sess.RevokeAll()
		if token, ok, _ := p.sess.Create(newKey, newKey, clientSource(r)); ok {
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   int(p.sess.ttl.Seconds()),
			})
		}
		log.Printf("[panel] api_key 已变更，全部旧会话已吊销")
	}
	log.Printf("[panel] 配置已保存（需重启生效的字段: %v）", restart)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restart})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// publicErr 给面板看的上游错误。HTML 错误页只留状态码，避免把整页塞进提示。
func publicErr(err error) string {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		msg := strings.ToLower(ue.Msg)
		if strings.Contains(msg, "<!doctype") || strings.Contains(msg, "<html") {
			return "上游返回 HTTP " + itoa(ue.Status)
		}
	}
	return clipText(err.Error(), 160)
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func atoi(s string) (int, bool) {
	n := 0
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<30 {
			return 1 << 30, true
		}
	}
	return n, true
}

func itoa(n int) string {
	if n < 0 {
		n = 0
	}
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	return string(b[i:])
}
