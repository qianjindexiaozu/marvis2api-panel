// session.go 面板登录会话：内存态 token + HttpOnly Cookie。
//
// 对齐 workbuddy2api-gui 的登录设计：登录页 + 服务端会话，替代浏览器
// prompt() 反复索要密钥的旧交互。初始密码即 config.json 的 api_key
// （单一凭证源，登录成功后浏览器持有 7 天滚动的会话 Cookie）。
//
// 脚本兼容：/panel/api/* 与 /admin/* 仍接受 `Authorization: Bearer <api_key>`
// 直连（curl / 自动化场景无需先登录）。
package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	sessionCookie = "mv2a_session"

	// sessionTTL 登录会话有效期（ sliding 不做，到期重登）。
	sessionTTL = 7 * 24 * time.Hour

	// maxLoginFailures / loginLockWindow 暴力破解防护：同源连续失败达上限后锁定。
	maxLoginFailures = 10
	loginLockWindow  = 10 * time.Minute
)

// session 一条已登录会话。
type session struct {
	expiresAt time.Time
}

// loginAttempt 单个来源的失败计数。
type loginAttempt struct {
	failures int
	firstAt  time.Time
}

// SessionStore 内存会话表（进程重启即失效，重新登录即可）。
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]session
	attempts map[string]loginAttempt
	ttl      time.Duration
}

// NewSessionStore 构建会话表（ttl<=0 用默认 7 天）。
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = sessionTTL
	}
	return &SessionStore{
		sessions: map[string]session{},
		attempts: map[string]loginAttempt{},
		ttl:      ttl,
	}
}

// Create 校验密码并创建会话。锁定中返回 ("" , false, 提示)。
func (s *SessionStore) Create(password, expect, source string) (string, bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 锁定检查：窗口内失败达上限则拒绝；窗口过期自动解锁。
	if a, ok := s.attempts[source]; ok {
		if a.failures >= maxLoginFailures {
			if time.Since(a.firstAt) < loginLockWindow {
				remain := (loginLockWindow - time.Since(a.firstAt)).Round(time.Second)
				return "", false, "失败次数过多，请 " + remain.String() + " 后再试"
			}
			delete(s.attempts, source)
		}
	}

	// 常量时间比较，避免时序侧信道泄露口令内容。
	if subtle.ConstantTimeCompare([]byte(password), []byte(expect)) != 1 {
		att := s.attempts[source]
		if att.failures == 0 || time.Since(att.firstAt) >= loginLockWindow {
			att = loginAttempt{firstAt: time.Now()}
		}
		att.failures++
		s.attempts[source] = att
		return "", false, "密码错误"
	}

	delete(s.attempts, source) // 成功即清空失败计数

	token, err := newSessionToken()
	if err != nil {
		return "", false, "生成会话失败"
	}
	s.sessions[token] = session{expiresAt: time.Now().Add(s.ttl)}
	s.gcLocked()
	return token, true, ""
}

// Validate 校验会话 token。
func (s *SessionStore) Validate(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(sess.expiresAt) {
		delete(s.sessions, token)
		return false
	}
	return true
}

// Revoke 注销会话。
func (s *SessionStore) Revoke(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// RevokeAll 吊销全部会话（api_key 修改后旧会话全部失效）。
func (s *SessionStore) RevokeAll() {
	s.mu.Lock()
	s.sessions = map[string]session{}
	s.mu.Unlock()
}

// gcLocked 清理过期会话。调用方必须已持锁。
func (s *SessionStore) gcLocked() {
	now := time.Now()
	for t, sess := range s.sessions {
		if now.After(sess.expiresAt) {
			delete(s.sessions, t)
		}
	}
}

func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// clientSource 取来源 IP（暴力破解计数维度）。
func clientSource(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// bearerToken 解析 Authorization: Bearer <token>。
func bearerToken(r *http.Request) string {
	const p = "Bearer "
	if v := r.Header.Get("Authorization"); len(v) > len(p) && v[:len(p)] == p {
		return v[len(p):]
	}
	return ""
}

// sessionCookieValue 读会话 Cookie（无则空串）。
func sessionCookieValue(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}
