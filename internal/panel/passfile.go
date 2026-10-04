package panel

import (
	"os"
	"path/filepath"
	"strings"
)

// PassFile 面板登录密码。和网关 API Key 分开存放。
type PassFile struct {
	path string
}

func NewPassFile(path string) *PassFile { return &PassFile{path: path} }

func (p *PassFile) Get() string {
	if p == nil || p.path == "" {
		return ""
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (p *PassFile) Set(password string) error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.TrimSpace(password)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}
