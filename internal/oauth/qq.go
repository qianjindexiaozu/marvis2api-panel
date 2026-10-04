package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// QQ 扫码走腾讯 ptlogin，不是微信那套 lp 长轮询。
// 官方客户端打开 graph.qq.com；这里在服务端取二维码并轮询，确认后用同一落地页换证。

const (
	qqClientID = "1903794758"
	qqPtAppID  = "716027609"
	qqDAID     = "383"
	qqJumpURL  = "https://graph.qq.com/oauth2.0/login_jump"
	qqUA       = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
)

// qqAuthorizeURL 确认后由登录页 POST 到这里换 code。测试可替换。
var qqAuthorizeURL = "https://graph.qq.com/oauth2.0/authorize"

var rePtui = regexp.MustCompile(`ptuiCB\('(\d+)','[^']*','([^']*)','[^']*','([^']*)'`)

// QQFlow QQ 扫码会话。零值不可用，用 NewQQFlow。
type QQFlow struct {
	HTTP      *http.Client
	AccessKey string // 启动时从 prepare 文件注入。

	mu       sync.Mutex
	sessions map[string]*qqSession
}

type qqSession struct {
	ID        string
	GUID      string
	QR        []byte
	CreatedAt time.Time
	client    *http.Client
	accessKey string

	mu   sync.Mutex
	stat string
	err  string
	nick string
	cred *Credential
}

// NewQQFlow 构建 QQ 扫码流程。
func NewQQFlow() *QQFlow {
	return &QQFlow{sessions: map[string]*qqSession{}}
}

// Start 拉取 QQ 登录二维码。每次使用新的 guid。
func (f *QQFlow) Start(ctx context.Context) (*qqSession, error) {
	guid := newGUID()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	redirect := landingBase + landingPath + "?login_type=QC"
	authURL := "https://graph.qq.com/oauth2.0/authorize?" + url.Values{
		"which":         {"Login"},
		"display":       {"pc"},
		"response_type": {"code"},
		"client_id":     {qqClientID},
		"redirect_uri":  {redirect},
		"state":         {guid},
	}.Encode()
	if _, err := qqGet(ctx, client, authURL, "https://graph.qq.com/"); err != nil {
		return nil, fmt.Errorf("打开 QQ 登录页失败: %w", err)
	}
	xlogin := "https://xui.ptlogin2.qq.com/cgi-bin/xlogin?" + url.Values{
		"appid": {qqPtAppID}, "daid": {qqDAID}, "style": {"33"},
		"login_text": {"登录"}, "hide_title_bar": {"1"}, "hide_border": {"1"},
		"target": {"self"}, "s_url": {qqJumpURL}, "pt_3rd_aid": {qqClientID}, "theme": {"2"},
	}.Encode()
	_, _ = qqGet(ctx, client, xlogin, "https://graph.qq.com/")
	qrURL := "https://ssl.ptlogin2.qq.com/ptqrshow?" + url.Values{
		"appid":      {qqPtAppID},
		"e":          {"2"},
		"l":          {"M"},
		"s":          {"3"},
		"d":          {"72"},
		"v":          {"4"},
		"t":          {fmt.Sprintf("%d", time.Now().UnixNano())},
		"daid":       {qqDAID},
		"pt_3rd_aid": {qqClientID},
		"u1":         {qqJumpURL},
	}.Encode()
	qr, err := qqGet(ctx, client, qrURL, "https://xui.ptlogin2.qq.com/")
	if err != nil {
		return nil, fmt.Errorf("下载 QQ 二维码失败: %w", err)
	}
	if len(qr) < 8 || string(qr[:8]) != "\x89PNG\r\n\x1a\n" {
		return nil, fmt.Errorf("QQ 二维码不是 PNG")
	}
	s := &qqSession{
		ID: newGUID(), GUID: guid, QR: qr, CreatedAt: time.Now(),
		client: client, accessKey: f.AccessKey, stat: StatusWaiting,
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) >= maxSessions {
		var oldest *qqSession
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

// Poll 查一次二维码状态。确认后自动换证。
func (f *QQFlow) Poll(ctx context.Context, id string) (string, error) {
	s := f.session(id)
	if s == nil {
		return "", fmt.Errorf("会话不存在")
	}
	s.mu.Lock()
	stat := s.stat
	s.mu.Unlock()
	if stat == StatusDone || stat == StatusExpired || stat == StatusError {
		return stat, nil
	}
	qrsig := cookieValue(s.client.Jar, "https://ssl.ptlogin2.qq.com/", "qrsig")
	if qrsig == "" {
		s.fail("二维码会话丢失，请重新扫码")
		return StatusError, nil
	}
	pollURL := "https://ssl.ptlogin2.qq.com/ptqrlogin?" + url.Values{
		"u1":         {qqJumpURL},
		"ptqrtoken":  {fmt.Sprintf("%d", hash33(qrsig))},
		"ptredirect": {"0"},
		"h":          {"1"},
		"t":          {"1"},
		"g":          {"1"},
		"from_ui":    {"1"},
		"ptlang":     {"2052"},
		"action":     {"0-0-" + fmt.Sprintf("%d", time.Now().UnixMilli())},
		"js_ver":     {"26092315"},
		"js_type":    {"1"},
		"login_sig":  {cookieValue(s.client.Jar, "https://xui.ptlogin2.qq.com/", "pt_login_sig")},
		"pt_uistyle": {"40"},
		"aid":        {qqPtAppID},
		"daid":       {qqDAID},
		"pt_3rd_aid": {qqClientID},
	}.Encode()
	raw, err := qqGet(ctx, s.client, pollURL, "https://xui.ptlogin2.qq.com/")
	if err != nil {
		return "", err
	}
	code, jump, msg := parsePtui(string(raw))
	if nick := parsePtuiNick(string(raw)); nick != "" {
		s.mu.Lock()
		s.nick = nick
		s.mu.Unlock()
	}
	switch code {
	case "66":
		s.setStat(StatusWaiting, "")
		return StatusWaiting, nil
	case "67":
		s.setStat(StatusScanned, "")
		return StatusScanned, nil
	case "65":
		s.setStat(StatusExpired, "二维码已失效，请重新扫码")
		return StatusExpired, nil
	case "0":
		if err := s.finish(ctx, jump); err != nil {
			s.fail(err.Error())
			return StatusError, nil
		}
		return StatusDone, nil
	default:
		if msg == "" {
			msg = "QQ 登录失败"
		}
		s.fail(msg)
		return StatusError, nil
	}
}

// Credential 返回已完成会话的凭证。
func (f *QQFlow) Credential(id string) *Credential {
	s := f.session(id)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cred
}

// Session 供面板取错误文本。返回的是带 Stat 的对象；不存在返回 nil。
func (f *QQFlow) Session(id string) *qqSession {
	return f.session(id)
}

func (s *qqSession) Stat() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stat, s.err
}

func (f *QQFlow) session(id string) *qqSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id]
}

func (s *qqSession) setStat(stat, errText string) {
	s.mu.Lock()
	s.stat = stat
	s.err = errText
	s.mu.Unlock()
}

func (s *qqSession) fail(msg string) {
	s.setStat(StatusError, msg)
}

func (s *qqSession) finish(ctx context.Context, jump string) error {
	if jump == "" {
		return fmt.Errorf("QQ 未返回跳转地址")
	}
	code, err := completeQQCode(ctx, s.client, jump, s.GUID)
	if err != nil {
		return err
	}
	flow := &Flow{HTTP: s.client, AccessKey: s.accessKey}
	sess := &Session{GUID: s.GUID, loginType: "QC"}
	if err := flow.exchange(ctx, sess, code); err != nil {
		return err
	}
	s.mu.Lock()
	s.cred = sess.Cred()
	if s.cred != nil && strings.TrimSpace(s.cred.Nickname) == "" {
		s.cred.Nickname = s.nick
	}
	if s.cred != nil && strings.TrimSpace(s.cred.Nickname) == "" {
		s.cred.Nickname = qqNickFromJar(s.client.Jar)
	}
	s.stat = StatusDone
	s.err = ""
	s.mu.Unlock()
	return nil
}

func completeQQCode(ctx context.Context, base *http.Client, rawURL, state string) (string, error) {
	if c := extractOAuthCode(rawURL); c != "" {
		return c, nil
	}
	body, err := followQQJump(ctx, base, rawURL)
	if c := extractOAuthCode(body); c != "" {
		return c, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	code, postErr := postQQAuthorize(ctx, base, state)
	if postErr != nil {
		if err != nil {
			return "", postErr
		}
		return "", postErr
	}
	return code, nil
}

func followQQJump(ctx context.Context, base *http.Client, rawURL string) (string, error) {
	var code string
	client := *base
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if c := extractOAuthCode(req.URL.String()); c != "" && strings.Contains(req.URL.Path, "marvis_oauth") {
			code = c
			return http.ErrUseLastResponse
		}
		if len(via) >= 8 {
			return fmt.Errorf("重定向过多")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", qqUA)
	req.Header.Set("Referer", "https://xui.ptlogin2.qq.com/")
	resp, err := client.Do(req)
	if err != nil && code == "" {
		return "", err
	}
	var body string
	if resp != nil {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		body = string(raw)
		if code == "" {
			code = extractOAuthCode(resp.Header.Get("Location"))
		}
	}
	if code != "" {
		return code, nil
	}
	return body, nil
}

func postQQAuthorize(ctx context.Context, base *http.Client, state string) (string, error) {
	pSkey := firstCookie(base.Jar, []string{"https://graph.qq.com/", "https://qq.com/", "https://ptlogin2.qq.com/"}, "p_skey")
	if pSkey == "" {
		pSkey = firstCookie(base.Jar, []string{"https://graph.qq.com/", "https://qq.com/"}, "skey")
	}
	ui := firstCookie(base.Jar, []string{"https://graph.qq.com/"}, "ui")
	if ui == "" {
		ui = strings.ToUpper(newGUID())
	}
	redirect := landingBase + landingPath + "?login_type=QC"
	form := url.Values{
		"response_type": {"code"},
		"client_id":     {qqClientID},
		"redirect_uri":  {redirect},
		"state":         {state},
		"from_ptlogin":  {"1"},
		"src":           {"1"},
		"update_auth":   {"1"},
		"openapi":       {"1010"},
		"g_tk":          {strconv.Itoa(gtk(pSkey))},
		"auth_time":     {strconv.FormatInt(time.Now().UnixMilli(), 10)},
		"ui":            {ui},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, qqAuthorizeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", qqUA)
	req.Header.Set("Origin", "https://graph.qq.com")
	req.Header.Set("Referer", "https://graph.qq.com/oauth2.0/show?which=Login&display=pc&client_id="+qqClientID)
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if c := extractOAuthCode(resp.Header.Get("Location")); c != "" {
		return c, nil
	}
	if c := extractOAuthCode(string(raw)); c != "" {
		return c, nil
	}
	var body struct {
		Ret      int    `json:"ret"`
		Msg      string `json:"msg"`
		Callback string `json:"callback"`
	}
	_ = json.Unmarshal(raw, &body)
	if c := extractOAuthCode(body.Callback); c != "" {
		return c, nil
	}
	if body.Msg != "" {
		return "", fmt.Errorf("QQ 授权失败: %s", body.Msg)
	}
	if pSkey == "" {
		return "", fmt.Errorf("QQ 确认后没有拿到登录态，请重新扫码")
	}
	return "", fmt.Errorf("QQ 确认后没有拿到授权码")
}

func extractOAuthCode(raw string) string {
	raw = strings.ReplaceAll(raw, `\/`, `/`)
	if u, err := url.Parse(raw); err == nil {
		if c := u.Query().Get("code"); c != "" {
			return c
		}
	}
	if m := reOAuthCode.FindStringSubmatch(raw); m != nil {
		c, err := url.QueryUnescape(m[1])
		if err != nil {
			return m[1]
		}
		return c
	}
	return ""
}

var reOAuthCode = regexp.MustCompile("code=([^&\"'\\s<>]+)")

func gtk(s string) int {
	hash := 5381
	for i := 0; i < len(s); i++ {
		hash += int(int32(hash)<<5) + int(s[i])
	}
	return hash & 0x7fffffff
}

func firstCookie(jar http.CookieJar, urls []string, name string) string {
	for _, raw := range urls {
		if v := cookieValue(jar, raw, name); v != "" {
			return v
		}
	}
	return ""
}

func qqGet(ctx context.Context, client *http.Client, rawURL, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", qqUA)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func parsePtui(s string) (code, jump, msg string) {
	m := rePtui.FindStringSubmatch(s)
	if m == nil {
		return "", "", s
	}
	return m[1], m[2], m[3]
}

// parsePtuiNick 取 ptuiCB 第 6 个参数。QQ 确认成功时这里是昵称。
func parsePtuiNick(s string) string {
	i := strings.Index(s, "ptuiCB(")
	if i < 0 {
		return ""
	}
	rest := s[i+len("ptuiCB("):]
	var args []string
	for len(args) < 6 {
		q := strings.Index(rest, "'")
		if q < 0 {
			break
		}
		rest = rest[q+1:]
		end := strings.Index(rest, "'")
		if end < 0 {
			break
		}
		args = append(args, rest[:end])
		rest = rest[end+1:]
	}
	if len(args) < 6 {
		return ""
	}
	nick, err := url.QueryUnescape(strings.TrimSpace(args[5]))
	if err != nil {
		nick = strings.TrimSpace(args[5])
	}
	return nick
}

func qqNickFromJar(jar http.CookieJar) string {
	if jar == nil {
		return ""
	}
	for _, rawURL := range []string{"https://ptlogin2.qq.com/", "https://graph.qq.com/", "https://qq.com/"} {
		for _, name := range []string{"ptnick", "nick", "nickname"} {
			v := cookieValue(jar, rawURL, name)
			if v == "" {
				continue
			}
			if dec, err := url.QueryUnescape(v); err == nil && dec != "" {
				v = dec
			}
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func hash33(s string) int32 {
	var e int32
	for _, r := range s {
		e += (e << 5) + int32(r)
	}
	return e & 0x7fffffff
}

func cookieValue(jar http.CookieJar, rawURL, name string) string {
	if jar == nil {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	for _, c := range jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// NormalizeLoginType 把官方数字渠道码收成 WX / QC。
func NormalizeLoginType(v string) string {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "1", "QC":
		return "QC"
	case "2", "WX", "":
		return "WX"
	default:
		return strings.ToUpper(strings.TrimSpace(v))
	}
}
