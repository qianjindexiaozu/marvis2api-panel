// Package upstream 封装对 Marvis 上游（chat / models / quota / auth）的全部
// HTTP 调用，以及错误分类（驱动 pool 冷却状态机）。
//
// 上游契约（真实端点，自 Marvis 桌面客户端解密配置确认并实测）：
// 对话按 OpenAI 兼容形态接入（SSE 流式 / JSON 非流式），鉴权走
// Ual-Access-* 头族（Access-Token / Guid / Openid / Login-Type / Requestid
// + MarvisExt 设备 JSON），对话链路另需 Marvis-* 路由头。上游的具体端点
// 常量集中在 Client 字段（BaseURL / ChatPath / ModelsPath / QuotaPath /
// RefreshPath），可由 config upstream.* 覆盖——上游协议演进时只改配置，
// 不动代码。
package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/logfmt"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/ual"
)

// 默认上游端点（均可被 config 覆盖）。
// 真实上游（从 Marvis 桌面客户端 1.0.0.10371 解密配置确认，2026-09-27 实测通过）：
//   - 对话：https://yybadaccess.3g.qq.com/v1/chat/completions（OpenAI 兼容，
//     鉴权走 Ual-Access-* 头族而非 Bearer；Content-Type 恒为 text/plain，
//     流式判读看出站请求的 stream 标志）；
//   - 每日额度：https://yybadaccess.3g.qq.com/v3/marvis_get_wallet（UAL md5 签名，
//     见 wallet.go）；
//   - 上游没有 /v1/models 端点：模型目录用真实客户端内置目录（models.go）。
const (
	defaultBaseURL    = "https://yybadaccess.3g.qq.com"
	defaultChatPath   = "/v1/chat/completions"
	defaultModelsPath = "/v1/models"
)

// marvisLoginType / marvisAgentTag / marvisScene LLM 链路固定头值
// （与官方客户端一致；scene=agent_chat 是客户端 agent 对话的固定场景）。
const (
	marvisLoginType = "6"
	marvisAgentTag  = "main"
	marvisScene     = "agent_chat"
)

// Client 上游 HTTP 客户端。零外部依赖，HTTP/2 由 net/http 自动协商。
type Client struct {
	// HTTP 短 RPC（models / quota / refresh）用：总时长受限。
	HTTP *http.Client
	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），
	// 首字节由 HeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	ChatHTTP *http.Client

	// HeaderTimeout chat SSE 响应头（首字节）上限。
	HeaderTimeout time.Duration
	// IdleTimeout chat SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// BaseURL 上游根地址（账号 BaseURL 覆盖优先）。
	BaseURL string
	// ChatPath / ModelsPath / QuotaPath / RefreshPath 上游端点路径。
	// QuotaPath 为空时改查 Marvis 每日额度（walletQuota）。
	// RefreshPath 为空 = 不刷新 token。
	ChatPath    string
	ModelsPath  string
	QuotaPath   string
	RefreshPath string
	// WalletURL 每日额度地址。空 = https://yybadaccess.3g.qq.com/v3/marvis_get_wallet。
	WalletURL string
	// DeviceQIMEI 本机 Beacon 注册的 qimei36。对话风控认这个，不认扫码时生成的登录 guid。
	DeviceQIMEI string
	// AccessKey 启动时从私有 prepare 文件加载，不内置默认值。
	AccessKey string

	// UserAgent 出站 UA（空 = Go 默认）。
	UserAgent string

	// SanitizeFingerprints 出站请求体指纹脱敏开关（默认 true）。
	SanitizeFingerprints bool

	// extraFingerprints 面板/配置注入的额外脱敏词（大小写不敏感子串替换）。
	extraFingerprints []string

	// modelsCache 动态模型目录缓存（1h TTL + 5min 负缓存），见 models.go。
	modelsMu       sync.RWMutex
	modelsCache    []ModelInfo
	modelsCachedAt time.Time
	modelsFailedAt time.Time
}

// ErrUnsupported 上游能力未接入（quota/refresh 路径未配置）。
var ErrUnsupported = errors.New("upstream capability not configured")

// New 生产默认值。HTTP 与 ChatHTTP 共享同一硬化 Transport（见 transport.go）。
func New() *Client {
	tr := newTransport(120 * time.Second)
	return &Client{
		HTTP: &http.Client{
			Timeout:   30 * time.Second,
			Transport: tr,
		},
		// 无总时长；首字节/空闲单独管。
		ChatHTTP:             &http.Client{Transport: tr},
		HeaderTimeout:        120 * time.Second,
		BaseURL:              defaultBaseURL,
		ChatPath:             defaultChatPath,
		ModelsPath:           defaultModelsPath,
		SanitizeFingerprints: true,
	}
}

// SetHeaderTimeout 调整 chat 首字节上限（装配阶段调用；同步改共享 Transport 的
// ResponseHeaderTimeout，两个 client 同时生效）。
func (c *Client) SetHeaderTimeout(d time.Duration) {
	c.HeaderTimeout = d
	if tr, ok := c.chatHTTP().Transport.(*http.Transport); ok && d > 0 {
		tr.ResponseHeaderTimeout = d
	}
}

// SetExtraFingerprints 注入额外脱敏词（启动/面板热更新调用）。
func (c *Client) SetExtraFingerprints(list []string) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	c.extraFingerprints = append([]string(nil), list...)
}

// baseURLFor 账号级 base_url 覆盖优先。
func (c *Client) baseURLFor(a *account.Auth) string {
	if a != nil {
		if b := strings.TrimRight(strings.TrimSpace(a.BaseURL), "/"); b != "" {
			return b
		}
	}
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

// ChatResult 上游响应的原始形态：状态码 + 头 + 未读 body（调用方负责 Close）。
type ChatResult struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// chatEndpoint 出站 chat 完整 URL。
func (c *Client) chatEndpoint(a *account.Auth) string {
	path := c.ChatPath
	if path == "" {
		path = defaultChatPath
	}
	return c.baseURLFor(a) + path
}

// modelsEndpoint 出站 models 完整 URL。
func (c *Client) modelsEndpoint(a *account.Auth) string {
	path := c.ModelsPath
	if path == "" {
		path = defaultModelsPath
	}
	return c.baseURLFor(a) + path
}

// Chat 转发一次对话请求。body 为最终出站 JSON（PrepareBody 产物）；
// clientHeaders 目前不透传任何客户端头（隔离指纹；未来按白名单放开）。
//
// 流式语义：上游 SSE 直接把 Body 交给调用方边读边推；本函数不做聚合，
// 也不缓存——首字节延迟由 HeaderTimeout 兜底，流中空闲由 IdleTimeout 兜底，
// 客户端断连由 ctx 取消兜底。
func (c *Client) Chat(ctx context.Context, a *account.Auth, body []byte) (*ChatResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatEndpoint(a), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if IsStreamBody(body) {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	c.applyCommonHeaders(req, a)
	c.applyChatHeaders(req)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		// 传输层失败 → 清空共享连接池的空闲连接（失败连接可能仍留在空闲池里）。
		closeIdle(c.chatHTTP().Transport)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		// 错误响应统一读完再交分类（chat 错误页通常很小；1MiB 防护）。
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			return nil, fmt.Errorf("read error body: %w", rerr)
		}
		kind := Classify(resp.StatusCode, string(raw))
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: logfmt.Truncate(string(raw), 200)}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			ue.RetryAfter = d
		}
		if kind == ErrNone {
			// 防御分支（≥400 不应产生 None）：交回原文让调用方兜底。
			return &ChatResult{StatusCode: resp.StatusCode, Header: resp.Header, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
		}
		return nil, ue
	}
	// 成功分支：IdleTimeout 监控接管 body。
	return &ChatResult{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       monitorBody(resp.Body, c.IdleTimeout),
	}, nil
}

// applyCommonHeaders 出站公共头（UAL 鉴权头族 + Marvis 路由头 + MarvisExt 设备头）。
// 真实上游不认 Bearer：登录态经 Ual-Access-Access-Token 携带，Ual-Access-Guid
// 与 Ual-Access-MarvisExt（含 guid 的设备信息 JSON）缺一会被风控拦截
// （403 code=4100404）或判未登录（401 code=4100403）。
func (c *Client) applyCommonHeaders(req *http.Request, a *account.Auth) {
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	var token, guid, openid string
	if a != nil {
		token = a.TokenValue()
		guid = strings.TrimSpace(a.Guid)
		openid = strings.TrimSpace(a.UID)
	}
	if token != "" {
		req.Header.Set("Ual-Access-Access-Token", token)
	}
	if guid != "" {
		req.Header.Set("Ual-Access-Guid", guid)
	}
	if openid != "" {
		req.Header.Set("Ual-Access-Openid", openid)
	}
	req.Header.Set("Ual-Access-Login-Type", marvisLoginType)
	req.Header.Set("Ual-Access-Requestid", randomID())
	if guid != "" {
		req.Header.Set("Ual-Access-MarvisExt", c.marvisExtHeader(guid, openid))
	}
}

// applyChatHeaders chat 专属路由头（对话链路额外要求 Marvis-* 三元组）。
func (c *Client) applyChatHeaders(req *http.Request) {
	convID, respID := randomID(), randomID()
	req.Header.Set("Marvis-AgentTag", marvisAgentTag)
	req.Header.Set("Marvis-Conversation-ID", convID)
	req.Header.Set("Marvis-Response-ID", respID)
	req.Header.Set("Marvis-Scene", marvisScene)
}

// marvisExtHeader 构造 Ual-Access-MarvisExt。
// guid 是这次登录绑定的设备号。qimei36 必须是 Beacon 注册过的 36 位设备号；
// 扫码生成的 UUID 放进 qimei36 会被 4100404 拦截。uskey 留空即可。
func (c *Client) marvisExtHeader(guid, openid string) string {
	qimei := guid
	if c != nil && strings.TrimSpace(c.DeviceQIMEI) != "" {
		qimei = strings.TrimSpace(c.DeviceQIMEI)
	}
	ext := map[string]any{
		"client_ip":               "",
		"client_platform":         "macos",
		"client_platform_version": "27.0.0",
		"client_qimei36":          qimei,
		"client_version":          "1.0.0.10371",
		"env_vendor":              "",
		"guid":                    guid,
		"is_ioa":                  false,
		"manufacturer":            "Apple",
		"nonce":                   randomID(),
		"os_version":              "macOS 27.0.0",
		"origin_openid":           openid,
		"phone_type":              "mac",
		"qimei36":                 qimei,
		"req_scene":               1,
		"sub_scene":               "",
		"task_type":               "user",
		"uskey":                   "",
	}
	raw, err := json.Marshal(ext)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// randomID 生成 32 位十六进制随机串（对话/响应 ID 与 Requestid 用）。
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// QuotaResponse 配额查询结果。
type QuotaResponse struct {
	Remaining int64 `json:"remaining"`
	Total     int64 `json:"total"`
}

// Quota 查询账号配额（余额）。QuotaPath 未配置时返回 ErrUnsupported。
// 响应解析尽力兼容两种形态：
//   - {"remaining": n, "total": m}
//   - {"code":0,"data":{"remaining":n,"total":m}}（信封形态，自动剥壳）
func (c *Client) Quota(ctx context.Context, a *account.Auth) (QuotaResponse, error) {
	if c.QuotaPath == "" {
		return c.walletQuota(ctx, a)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURLFor(a)+c.QuotaPath, nil)
	if err != nil {
		return QuotaResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	c.applyCommonHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return QuotaResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QuotaResponse{}, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return QuotaResponse{}, &Error{Kind: kind, Status: resp.StatusCode, Msg: logfmt.Truncate(string(raw), 200)}
	}
	q, err := parseQuota(raw)
	if err != nil {
		return QuotaResponse{}, err
	}
	return q, nil
}

// parseQuota 配额响应解析（直返形态 / 信封形态双兼容）。
func parseQuota(raw []byte) (QuotaResponse, error) {
	var direct QuotaResponse
	if err := json.Unmarshal(raw, &direct); err == nil && (direct.Remaining > 0 || direct.Total > 0) {
		return direct, nil
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		var inner QuotaResponse
		if err := json.Unmarshal(env.Data, &inner); err == nil {
			return inner, nil
		}
		// data 形态二：{"quota":{"remaining":..}} / {"balance":n}
		var loose struct {
			Remaining *int64 `json:"remaining"`
			Total     *int64 `json:"total"`
			Balance   *int64 `json:"balance"`
		}
		if err := json.Unmarshal(env.Data, &loose); err == nil {
			if loose.Remaining != nil {
				inner.Remaining = *loose.Remaining
			}
			if loose.Balance != nil && inner.Remaining == 0 {
				inner.Remaining = *loose.Balance
			}
			if loose.Total != nil {
				inner.Total = *loose.Total
			}
			if inner.Remaining != 0 || inner.Total != 0 {
				return inner, nil
			}
		}
	}
	return QuotaResponse{}, fmt.Errorf("quota 响应无法解析: %s", logfmt.Truncate(string(raw), 120))
}

// refreshIOTimeout 刷新端点网络 I/O 上限。
const refreshIOTimeout = 30 * time.Second

// RefreshToken 刷新 access token（协议来自对官方客户端 H5 SDK 的还原，已实测）：
//
//	POST {base}/marvis_client/marvis_refresh_token
//	Cookie: openid=...; guid=...; refreshtoken=...   ← openid/guid 由 Cookie 读取
//	Ual-Access-Businessid: marvis_client
//	Ual-Access-Timestamp / Nonce / Signature（md5(body+ts+accessKey+nonce)）
//	Body: {"userInfo":{"openId","refreshToken","accessToken","loginType"}}
//
// 成功响应 {"code":0,"user_info":{"access_token","expires_in"}}。refresh_token
// 不轮换；access_token 全新（WX 渠道有效期 7200s）。成功时经 a.WriteBack 写回
// 并记录 ExpiresAt（调用方负责 Save）。
func (c *Client) RefreshToken(ctx context.Context, a *account.Auth) error {
	if c.RefreshPath == "" {
		return ErrUnsupported
	}
	rt := strings.TrimSpace(a.RefreshTokenValue())
	if rt == "" {
		return fmt.Errorf("no refresh_token")
	}
	openid := strings.TrimSpace(a.UID)
	guid := strings.TrimSpace(a.Guid)
	if openid == "" || guid == "" {
		return fmt.Errorf("refresh 需要 openid 与 guid（登录渠道 Cookie 里取）")
	}
	payload, err := json.Marshal(map[string]any{
		"userInfo": map[string]string{
			"openId":       openid,
			"refreshToken": rt,
			"accessToken":  a.TokenValue(),
			"loginType":    a.LoginType,
		},
	})
	if err != nil {
		return err
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := fmt.Sprintf("%d", randIntN(10000))
	sig, err := ual.Sign(payload, ts, nonce, c.AccessKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURLFor(a)+c.RefreshPath, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ual-Access-Businessid", marvisBusinessID)
	req.Header.Set("Ual-Access-Timestamp", ts)
	req.Header.Set("Ual-Access-Nonce", nonce)
	req.Header.Set("Ual-Access-Signature", sig)
	req.Header.Set("Cookie", fmt.Sprintf("openid=%s; guid=%s; refreshtoken=%s", openid, guid, rt))
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return &Error{Kind: kind, Status: resp.StatusCode, Msg: logfmt.Truncate(string(raw), 200)}
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		User *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    any    `json:"expires_in"`
		} `json:"user_info"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("refresh 响应无法解析: %s", logfmt.Truncate(string(raw), 120))
	}
	if env.Code != 0 {
		return fmt.Errorf("refresh 失败 code=%d msg=%s", env.Code, env.Msg)
	}
	if env.User == nil || env.User.AccessToken == "" {
		return fmt.Errorf("refresh 响应缺少 access_token")
	}
	a.WriteBack(env.User.AccessToken, env.User.RefreshToken)
	if n, ok := toInt64(env.User.ExpiresIn); ok && n > 0 {
		a.SetExpiresAt(time.Now().Unix() + n)
	}
	return nil
}

// randIntN [0,n) 随机数（nonce 用，官方为 0-9999 整数）。
func randIntN(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// toInt64 宽松数字解析（expires_in 可能是数字或字符串）。
func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	}
	return 0, false
}

// IsStreamBody 判断出站 body 是否声明了流式（stream:true）。
// 导出：真实上游响应 Content-Type 恒为 text/plain，调用方只能以出站
// stream 标志决定按 SSE 透传还是 JSON 聚合。
// 解析失败按非流式处理（只影响 Accept 头的礼貌值，不影响功能）。
func IsStreamBody(body []byte) bool {
	var obj struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	return obj.Stream
}

// closeIdle 清空传输层空闲连接池（传输层失败后调用，断根复用坏连接）。
func closeIdle(tr http.RoundTripper) {
	if ci, ok := tr.(interface{ CloseIdleConnections() }); ok {
		ci.CloseIdleConnections()
	}
}

// monitorBody 包装 SSE body：流中空闲超过 idle 无数据即取消底层请求（断流）。
// 活跃吐数据续命不掐。idle <= 0 时原样返回底流（禁用监控）。
func monitorBody(rc io.ReadCloser, idle time.Duration) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	mb := &monitorReader{rc: rc, idle: idle}
	mb.timer = time.AfterFunc(idle, func() {
		mb.mu.Lock()
		defer mb.mu.Unlock()
		if !mb.closed {
			_ = mb.rc.Close() // 掐断底层连接：Read 返回错误，调用方按传输层错误处置
			mb.timedOut = true
		}
	})
	return mb
}

// monitorReader 带空闲重置计时的 ReadCloser。
type monitorReader struct {
	rc       io.ReadCloser
	idle     time.Duration
	timer    *time.Timer
	mu       sync.Mutex
	closed   bool
	timedOut bool
}

func (m *monitorReader) Read(p []byte) (int, error) {
	n, err := m.rc.Read(p)
	if n > 0 {
		m.timer.Reset(m.idle) // 活跃续命
	}
	return n, err
}

func (m *monitorReader) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	m.timer.Stop()
	return m.rc.Close()
}

// TimedOut 报告流是否因空闲超时被掐（调用方读出错后诊断用）。
func (m *monitorReader) TimedOut() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.timedOut
}
