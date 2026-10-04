package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Key 一把网关调用密钥。登录密码不在这里。
type Key struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used,omitempty"`
	LastCheck string `json:"last_check,omitempty"`
	LastOK    bool   `json:"last_ok,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

// Store 多密钥文件。权限 0600。
type Store struct {
	path string
	mu   sync.Mutex
	keys []Key
}

func NewStore(path string) *Store {
	s := &Store{path: path}
	_ = s.load()
	return s
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var keys []Key
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	s.keys = keys
	return nil
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.keys, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// EnsureLegacy 把配置里的旧 api_key 收进列表，避免拆开密码后原来的调用突然失效。
func (s *Store) EnsureLegacy(secret string) {
	secret = strings.TrimSpace(secret)
	if s == nil || secret == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.Secret == secret {
			return
		}
	}
	s.keys = append(s.keys, Key{
		ID: "legacy", Name: "配置中的密钥", Secret: secret,
		CreatedAt: time.Now().Format(time.RFC3339),
	})
	_ = s.save()
}

// Create 生成一把新密钥。返回值含完整 secret，只在创建时给前端看一次。
func (s *Store) Create(name string) (Key, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return Key{}, err
	}
	k := Key{
		ID:        hex.EncodeToString(buf[:4]),
		Name:      strings.TrimSpace(name),
		Secret:    "mv-" + hex.EncodeToString(buf),
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	if k.Name == "" {
		k.Name = "未命名"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	if err := s.save(); err != nil {
		s.keys = s.keys[:len(s.keys)-1]
		return Key{}, err
	}
	return k, nil
}

func (s *Store) List() []Key {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, len(s.keys))
	copy(out, s.keys)
	return out
}

func (s *Store) Get(id string) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

func (s *Store) SetDisabled(id string, disabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID == id {
			s.keys[i].Disabled = disabled
			_ = s.save()
			return true
		}
	}
	return false
}

func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID != id {
			continue
		}
		s.keys = append(s.keys[:i], s.keys[i+1:]...)
		_ = s.save()
		return true
	}
	return false
}

// HasEnabled 是否至少有一把启用的密钥。
func (s *Store) HasEnabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if !k.Disabled && k.Secret != "" {
			return true
		}
	}
	return false
}

// Accept 常量时间比对已启用的密钥。
func (s *Store) Accept(token string) bool {
	if s == nil || token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := false
	for i := range s.keys {
		if s.keys[i].Disabled {
			continue
		}
		if subtle.ConstantTimeCompare(digest(token), digest(s.keys[i].Secret)) == 1 {
			ok = true
		}
	}
	return ok
}

func (s *Store) NoteCheck(id string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID == id {
			s.keys[i].LastCheck = time.Now().Format(time.RFC3339)
			s.keys[i].LastOK = ok
			_ = s.save()
			return
		}
	}
}

func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// Mask 列表展示：只留首尾，中间用星号。
func Mask(secret string) string {
	if len(secret) <= 8 {
		return "****"
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}

// FirstEnabled 返回一把启用中的密钥，供面板内部调用网关。没有则空。
func (s *Store) FirstEnabled() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if !k.Disabled && k.Secret != "" && k.ID != "legacy" {
			return k.Secret
		}
	}
	return ""
}
