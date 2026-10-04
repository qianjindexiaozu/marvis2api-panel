// Package oauth 实现不依赖官方客户端的在线扫码授权：网关在服务端驱动
// 微信开放平台的标准 PC 二维码登录流程，用户在面板里扫码确认后，code 经
// Marvis 登录后端落地页换取凭证（accesstoken/refreshtoken/openid/guid），
// 直接落盘为 auth.json——桌面 App 不再参与。
//
// 协议还原自官方客户端 H5 SDK（OfflinePack treemap JS）并已实测：
//
//	① GET open.weixin.qq.com/connect/qrconnect?appid=…&redirect_uri=…&state=<guid>…
//	   → 页面内含二维码会话 uuid 与二维码图片地址；
//	② 长轮询 lp.open.weixin.qq.com/connect/l/qrconnect?uuid=…
//	   → window.wx_errcode=408(等待)/404(已扫)/405(确认+wx_code)/402(过期)；
//	③ GET {base}/marvis_client_login/marvis_oauth?login_type=WX&code=…&state=…
//	   → 服务端用 code 向微信换 token，Set-Cookie 下发全套凭证。
//
// 注意：redirect_uri 必须与登录后端换取 token 时所用形态逐字节一致
// （即 /marvis_client_login/marvis_oauth?login_type=WX，不带 state），
// 否则 code 换取失败返回 RC_PARAMS_INVALID。
package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/ual"
)

const (
	wxAppID = "wxb650708317d56f88" // Marvis 在微信开放平台的应用 id（客户端内置）

	requestReferer = "https://yybadaccess.3g.qq.com/"

	// lpHoldTimeout lp 端点服务端保持约 25s 后返回等待态，客户端超时要给足。
	lpHoldTimeout = 40 * time.Second
	// sessionTTL 二维码会话有效期（微信侧约 5 分钟，留裕量）。
	sessionTTL = 6 * time.Minute
	// maxSessions 同时存在的授权会话上限（防泄漏；单用户面板足够）。
	maxSessions = 8
)

// 外部端点（包级变量以便测试注入假服务）。生产值与官方客户端一致。
var (
	wxQRPage  = "https://open.weixin.qq.com/connect/qrconnect"
	wxQRImage = "https://open.weixin.qq.com/connect/qrcode/"
	wxLPPoll  = "https://lp.open.weixin.qq.com/connect/l/qrconnect"

	landingBase = "https://yybadaccess.3g.qq.com"
	landingPath = "/marvis_client_login/marvis_oauth" // login_type=WX 由调用处拼接
)

// 会话状态。
const (
	StatusWaiting  = "waiting"   // 等待扫码
	StatusScanned  = "scanned"   // 已扫码，等待手机端确认
	StatusDone     = "done"      // 授权完成，凭证已换取
	StatusExpired  = "expired"   // 二维码过期，需要重新发起
	StatusTimedOut = "timed_out" // 会话超时未完成
	StatusError    = "error"     // 流程错误（详见 ErrText）
)

// Credential 扫码授权成功后拿到的凭证（等价于官方客户端 Cookies 里的一套）。
type Credential struct {
	AccessToken  string
	RefreshToken string
	OpenID       string
	GUID         string
	LoginType    string
	Nickname     string
	ExpiresIn    int64 // access token 有效期（秒；来自 expires_in cookie，WX 为 7200）
}

// Session 一次扫码授权会话。
type Session struct {
	ID        string
	GUID      string // state 参数，同时作为登录后的设备 guid
	uuid      string // 微信二维码会话 id
	QR        []byte // 二维码 PNG
	CreatedAt time.Time

	mu        sync.Mutex
	stat      string
	err       string
	cred      *Credential
	jar       *cookiejar.Jar // 换证后的 cookie 上下文（check_login 复用）
	loginType string         // 空 = WX。QQ 扫码设为 QC。
}

// Stat 返回当前状态与错误文本。
func (s *Session) Stat() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stat, s.err
}

// Cred 返回换取的凭证（未完成时 nil）。
func (s *Session) Cred() *Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

// Flow 微信扫码授权流程。并发安全；零值不可用，用 NewFlow 构建。
type Flow struct {
	HTTP      *http.Client // 不跟随重定向的场景由内部临时 client 处理；nil 用默认
	AccessKey string       // 启动时从 prepare 文件注入，不内置钥匙。

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewFlow 构建流程管理器。
func NewFlow() *Flow {
	return &Flow{sessions: make(map[string]*Session)}
}

var (
	reQRUUID  = regexp.MustCompile(`qrcode/([a-zA-Z0-9_\-]+)`)
	reErrCode = regexp.MustCompile(`wx_errcode=(\d+)`)
	reWxCode  = regexp.MustCompile(`wx_code=['"]([^'"]*)['"]`)
	reWxNick  = regexp.MustCompile(`wx_nickname=['"]([^'"]*)['"]`)
)

// Start 发起新的扫码授权：拉取二维码页解析会话 uuid，下载二维码 PNG。
// existingGuid 非空时复用该设备 guid（官方客户端的语义：同一安装终生一个
// guid，跨登录复用以积累设备信任；仅首次接入时才生成新的）。
func (f *Flow) Start(ctx context.Context, existingGuid string) (*Session, error) {
	guid := strings.TrimSpace(existingGuid)
	if guid == "" {
		guid = newGUID()
	}
	// 与客户端 H5 SDK 一致：redirect_uri 为登录后端规范形态（不带 state），
	// state 作为 qrconnect 独立参数传入，微信回跳时原样带回。
	redirect := landingBase + landingPath + "?login_type=WX"
	q := url.Values{
		"appid":         {wxAppID},
		"redirect_uri":  {redirect},
		"response_type": {"code"},
		"scope":         {"snsapi_login"},
		"fast_login":    {"1"},
		"state":         {guid},
		"self_redirect": {"true"},
	}
	pageURL := wxQRPage + "?" + q.Encode() + "#wechat_redirect"

	body, err := f.get(ctx, pageURL, requestReferer)
	if err != nil {
		return nil, fmt.Errorf("拉取二维码页失败: %w", err)
	}
	m := reQRUUID.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("二维码页未包含会话 uuid（响应 %d 字节）", len(body))
	}
	qr, err := f.get(ctx, wxQRImage+string(m[1]), wxQRPage)
	if err != nil {
		return nil, fmt.Errorf("下载二维码失败: %w", err)
	}

	s := &Session{
		ID:        newGUID(),
		GUID:      guid,
		uuid:      string(m[1]),
		QR:        qr,
		CreatedAt: time.Now(),
		stat:      StatusWaiting,
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gcLocked()
	if len(f.sessions) >= maxSessions {
		// 挤掉最老的一个
		var oldest *Session
		for _, x := range f.sessions {
			if oldest == nil || x.CreatedAt.Before(oldest.CreatedAt) {
				oldest = x
			}
		}
		delete(f.sessions, oldest.ID)
	}
	f.sessions[s.ID] = s
	return s, nil
}

// Poll 推进一次长轮询并返回当前状态。微信 lp 端点会在状态变化（或约 25s
// 超时）时返回；返回 waiting 时前端应继续调用。确认（405）后本方法自动
// 完成落地换证，之后再次查询直接返回 done。
func (f *Flow) Poll(ctx context.Context, id string) (string, error) {
	s := f.session(id)
	if s == nil {
		return "", fmt.Errorf("会话不存在")
	}
	if stat, _ := s.Stat(); stat == StatusDone || stat == StatusExpired || stat == StatusError {
		return stat, nil // 终态直接返回（done 的凭证从 Session.Cred 取）
	}
	if time.Since(s.CreatedAt) > sessionTTL {
		s.mu.Lock()
		s.stat = StatusTimedOut
		s.err = "二维码会话超时，请重新发起"
		s.mu.Unlock()
		return StatusTimedOut, nil
	}

	pctx, cancel := context.WithTimeout(ctx, lpHoldTimeout)
	defer cancel()
	body, err := f.get(pctx, fmt.Sprintf("%s?uuid=%s&_=%d", wxLPPoll, s.uuid, time.Now().UnixMilli()), wxQRPage)
	if err != nil {
		s.mu.Lock()
		s.stat = StatusError
		s.err = "长轮询失败: " + err.Error()
		s.mu.Unlock()
		return StatusError, nil
	}
	ec := ""
	if m := reErrCode.FindSubmatch(body); m != nil {
		ec = string(m[1])
	}
	switch ec {
	case "408": // 等待扫码
	case "404": // 已扫码待确认
		s.mu.Lock()
		if s.stat == StatusWaiting {
			s.stat = StatusScanned
		}
		s.mu.Unlock()
	case "405": // 确认，取 code 换凭证
		m := reWxCode.FindSubmatch(body)
		if m == nil || len(m[1]) == 0 {
			s.mu.Lock()
			s.stat = StatusError
			s.err = "确认回调缺少 code"
			s.mu.Unlock()
			return StatusError, nil
		}
		if err := f.exchange(ctx, s, string(m[1])); err != nil {
			s.mu.Lock()
			s.stat = StatusError
			s.err = err.Error()
			s.mu.Unlock()
			return StatusError, nil
		}
		if nick := wxNick(body); nick != "" {
			s.mu.Lock()
			if s.cred != nil && strings.TrimSpace(s.cred.Nickname) == "" {
				s.cred.Nickname = nick
			}
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.stat = StatusDone
		s.mu.Unlock()
	default: // 402/403 等过期类
		s.mu.Lock()
		s.stat = StatusExpired
		s.err = "二维码已失效，请重新扫码"
		s.mu.Unlock()
	}
	stat, _ := s.Stat()
	return stat, nil
}

// Credential 返回已完成会话的凭证（未完成返回 nil）。
func (f *Flow) Credential(id string) *Credential {
	s := f.session(id)
	if s == nil {
		return nil
	}
	return s.Cred()
}

// Session 返回会话句柄（不存在返回 nil）。
func (f *Flow) Session(id string) *Session {
	return f.session(id)
}

// exchange 用确认回调的 code 访问登录后端落地页换证（已实测协议）：
//
//	GET {base}/marvis_client_login/marvis_oauth?login_type=WX&code=…&state=…
//	Cookie: guid=<state>          ← 必需！缺 guid cookie 服务端报 RC_PARAMS_INVALID
//	→ HTTP 302（Location 指向 static.pc.yyb.qq.com 登录成功页）
//	  body {"code":0,"msg":"RC_SUCCESS"}
//	  Set-Cookie: accesstoken/refreshtoken/openid/logintype/expires_in/scope/appid/is_locked
//
// 凭证全部在第一跳的 Set-Cookie 里，无需真正跟随重定向。
func (f *Flow) exchange(ctx context.Context, s *Session, code string) error {
	loginType := s.loginType
	if loginType == "" {
		loginType = "WX"
	}
	landing := fmt.Sprintf("%s%s?login_type=%s&code=%s&state=%s", landingBase, landingPath, url.QueryEscape(loginType), url.QueryEscape(code), url.QueryEscape(s.GUID))

	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	if lu, err := url.Parse(landing); err == nil {
		// 预置登录 cookie。guid 是这次扫码生成的登录设备号，落地页缺它会报 RC_PARAMS_INVALID。
		// 它不是 Beacon 注册的 qimei36。对话风控认的是请求头里的已注册设备号，见 upstream.DeviceQIMEI。
		jar.SetCookies(lu, []*http.Cookie{
			{Name: "guid", Value: s.GUID, Path: "/"},
			{Name: "qimei36", Value: s.GUID, Path: "/"},
		})
	}
	client := *f.client()
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, landing, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("访问落地页: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &env)
	if env.Code != 0 {
		return fmt.Errorf("落地换证失败 code=%d msg=%s", env.Code, env.Msg)
	}

	cred := harvestCookies(jar, landingBase+landingPath)
	if cred.AccessToken == "" || cred.RefreshToken == "" || cred.OpenID == "" {
		return fmt.Errorf("落地页未下发完整凭证（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	cred.GUID = s.GUID // guid 即 state（服务端确认存在时不重发，以本地生成值为准）
	if cred.LoginType == "" {
		cred.LoginType = loginType
	}
	cred.LoginType = NormalizeLoginType(cred.LoginType)
	s.mu.Lock()
	s.cred = cred
	s.jar = jar
	s.mu.Unlock()

	// 复刻官方客户端时序：登录落地后立即调 check_login（会话激活/校验，
	// 返回值可能轮换 token，成功则采用）。失败不阻断——凭证已到手。
	f.checkLoginQuietly(ctx, s, jar, cred)
	return nil
}

// checkLoginQuietly 调 marvis_check_login（与 refresh 同构：UAL 签名 +
// cookie + {"userInfo":…} body）。官方客户端在 OAuth 落地后立即调用它。
func (f *Flow) checkLoginQuietly(ctx context.Context, s *Session, jar *cookiejar.Jar, cred *Credential) {
	defer func() { _ = recover() }() // 激活步骤绝不影响登录结果
	payload, err := json.Marshal(map[string]any{
		"userInfo": map[string]string{
			"openId":       cred.OpenID,
			"refreshToken": cred.RefreshToken,
			"accessToken":  cred.AccessToken,
			"loginType":    cred.LoginType,
		},
	})
	if err != nil {
		return
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := fmt.Sprintf("%d", randIntN(10000))
	sig, err := ual.Sign(payload, ts, nonce, f.AccessKey)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, landingBase+"/marvis_client/marvis_check_login", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ual-Access-Businessid", "marvis_client")
	req.Header.Set("Ual-Access-Timestamp", ts)
	req.Header.Set("Ual-Access-Nonce", nonce)
	req.Header.Set("Ual-Access-Signature", sig)
	client := *f.client()
	client.Jar = jar
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		User *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    any    `json:"expires_in"`
		} `json:"user_info"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Code != 0 || env.User == nil {
		return
	}
	s.mu.Lock()
	if env.User.AccessToken != "" {
		s.cred.AccessToken = env.User.AccessToken
	}
	if env.User.RefreshToken != "" {
		s.cred.RefreshToken = env.User.RefreshToken
	}
	if n, ok := toInt64(env.User.ExpiresIn); ok && n > 0 {
		s.cred.ExpiresIn = n
	}
	s.mu.Unlock()
}

// harvestCookies 从 cookie jar 中按名提取凭证字段（大小写不敏感）。
func wxNick(body []byte) string {
	m := reWxNick.FindSubmatch(body)
	if m == nil {
		return ""
	}
	nick, err := url.QueryUnescape(strings.TrimSpace(string(m[1])))
	if err != nil {
		return strings.TrimSpace(string(m[1]))
	}
	return nick
}

func harvestCookies(jar *cookiejar.Jar, rawURL string) *Credential {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	cred := &Credential{}
	for _, c := range jar.Cookies(u) {
		switch strings.ToLower(c.Name) {
		case "accesstoken":
			cred.AccessToken = c.Value
		case "refreshtoken":
			cred.RefreshToken = c.Value
		case "openid":
			cred.OpenID = c.Value
		case "guid":
			cred.GUID = c.Value
		case "logintype":
			cred.LoginType = strings.ToUpper(c.Value)
		case "nickname":
			cred.Nickname = c.Value
		case "expires_in":
			if n, err := strconv.ParseInt(strings.TrimSpace(c.Value), 10, 64); err == nil && n > 0 {
				cred.ExpiresIn = n
			}
		}
	}
	return cred
}

func (f *Flow) client() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return http.DefaultClient
}

func (f *Flow) get(ctx context.Context, rawURL, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (f *Flow) session(id string) *Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id]
}

// gcLocked 清理过期会话（调用方持锁）。
func (f *Flow) gcLocked() {
	for id, s := range f.sessions {
		if time.Since(s.CreatedAt) > sessionTTL+5*time.Minute {
			delete(f.sessions, id)
		}
	}
}

func newGUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // uuid v4
	b[8] = (b[8] & 0x3f) | 0x80
	// 与官方客户端一致：36 位带连字符的标准 UUID 形态
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
