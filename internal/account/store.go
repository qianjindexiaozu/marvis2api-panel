package account

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store 多账号凭证目录。每个账号一个 0600 JSON，不互相覆盖。
type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Dir() string { return s.dir }

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, fileName(id))
}

// List 读出全部账号。坏文件跳过并返回第一个错误。
func (s *Store) List() ([]*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "_") {
			continue
		}
		a, err := NewFile(filepath.Join(s.dir, name)).Load()
		if err != nil || a == nil {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// Save 按 openid（或 guid）落盘。同号再次登录会更新这一份，不会新增一条。
func (s *Store) Save(a *Auth) error {
	if a == nil || a.ID() == "" {
		return os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return NewFile(s.path(a.ID())).Save(a)
}

// Load 按 id、openid 或文件名读取。
func (s *Store) Load(id string) (*Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := NewFile(s.path(id)).Load()
	if err == nil && a != nil {
		return a, nil
	}
	entries, rerr := os.ReadDir(s.dir)
	if rerr != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		got, lerr := NewFile(filepath.Join(s.dir, e.Name())).Load()
		if lerr != nil || got == nil {
			continue
		}
		if got.ID() == id || got.UID == id || got.Label() == id {
			return got, nil
		}
	}
	if err == nil {
		return nil, os.ErrNotExist
	}
	return nil, err
}

// Delete 删除一个账号文件。不存在视为成功。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) SetActive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, "_active"), []byte(id), 0o600)
}

func (s *Store) Active() string {
	b, err := os.ReadFile(filepath.Join(s.dir, "_active"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Import 把旧的单文件凭证放进目录。同 id 已存在则不覆盖。
func (s *Store) Import(a *Auth) error {
	if a == nil || a.ID() == "" {
		return nil
	}
	if _, err := os.Stat(s.path(a.ID())); err == nil {
		return nil
	}
	return s.Save(a)
}

func fileName(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" || name == "." || name == ".." {
		name = "account"
	}
	return name + ".json"
}
