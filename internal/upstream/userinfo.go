package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/ual"
)

// UserInfo 读取微信或 QQ 的展示名。登录落地页不带昵称，官方客户端登录后另请求这个接口。
func (c *Client) UserInfo(ctx context.Context, a *account.Auth) (string, error) {
	if c == nil || a == nil || a.TokenValue() == "" || strings.TrimSpace(a.UID) == "" {
		return "", fmt.Errorf("缺少 token 或 openid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURLFor(a)+"/marvis_client/marvis_get_user_info", nil)
	if err != nil {
		return "", err
	}
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	nonce := fmt.Sprintf("%d", randIntN(10000))
	sig, err := ual.Sign(nil, ts, nonce, c.AccessKey)
	if err != nil {
		return "", err
	}
	req.Header.Set("Ual-Access-Businessid", marvisBusinessID)
	req.Header.Set("Ual-Access-Timestamp", ts)
	req.Header.Set("Ual-Access-Nonce", nonce)
	req.Header.Set("Ual-Access-Signature", sig)
	req.Header.Set("Ual-Access-Access-Token", a.TokenValue())
	req.Header.Set("Ual-Access-Openid", strings.TrimSpace(a.UID))
	req.Header.Set("Ual-Access-Login-Type", userInfoLoginType(a.LoginType))
	if guid := strings.TrimSpace(a.Guid); guid != "" {
		req.Header.Set("Ual-Access-Guid", guid)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	nick, ok := parseNick(raw)
	if ok && nick != "" && nick != strings.TrimSpace(a.UID) {
		return nick, nil
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("用户信息 HTTP %d", resp.StatusCode)
	}
	return "", userInfoErr(raw)
}

func userInfoLoginType(v string) string {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "1", "QC":
		return "1"
	default:
		return "2"
	}
}

func userInfoErr(raw []byte) error {
	var env struct {
		Ret int    `json:"ret"`
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(raw, &env)
	msg := strings.TrimSpace(env.Msg)
	if msg == "" {
		return fmt.Errorf("用户信息没有名称")
	}
	if len(msg) > 80 {
		msg = msg[:80]
	}
	return fmt.Errorf("%s", msg)
}

func parseNick(raw []byte) (string, bool) {
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return "", false
	}
	if n := nickFrom(env); n != "" {
		return n, true
	}
	if data, ok := env["data"].(map[string]any); ok {
		if n := nickFrom(data); n != "" {
			return n, true
		}
	}
	if user, ok := env["user_info"].(map[string]any); ok {
		if n := nickFrom(user); n != "" {
			return n, true
		}
	}
	return "", false
}

func nickFrom(m map[string]any) string {
	for _, k := range []string{"nick_name", "nickname", "nickName"} {
		s, _ := m[k].(string)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if dec, err := url.QueryUnescape(s); err == nil && dec != "" {
			s = dec
		}
		return s
	}
	return ""
}
