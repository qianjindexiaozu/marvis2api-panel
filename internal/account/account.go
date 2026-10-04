// Package account Marvis 账号凭证的解析、校验与落盘。
//
// 旧的单文件凭证在 data/auth.json。多账号在同目录 accounts/ 下，一个号一个文件。
// 文件权限 0600，tmp + rename 原子替换。
//
// 安全约束：
//   - 文件权限 0600（token 等同上游账号的操作员凭据）；
//   - 原子替换：先写 <file>.tmp 再 rename，避免半写文件被加载；
//   - token 刷新（配置了 upstream.refresh_path 时）经 Save 以 0600 原子写回。
package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 单个 Marvis 账号的凭证与元信息。
//
// 内嵌互斥锁保护 token 字段（refresh 在锁内改写、chat 读取经快照方法）。
// Auth 含锁，**禁止整体拷贝**（go vet copies-lock）：需要快照时用 Clone()。
type Auth struct {
	mu sync.Mutex

	// Name 人读名称（面板展示；缺省取 account.nickname）。
	Name string `json:"-"`
	// Token 上游访问凭证。必填。
	Token string `json:"-"`
	// RefreshToken 可选刷新凭证（配置 upstream.refresh_path 后可用；空 = 不刷新）。
	RefreshToken string `json:"-"`
	// ExpiresAt access token 过期时刻（Unix 秒；0 = 未知）。
	ExpiresAt int64 `json:"-"`
	// BaseURL 上游根地址覆盖（空 = 用全局 upstream.base_url）。
	BaseURL string `json:"-"`
	// Guid 客户端设备标识。查每日额度时放在 Ual-Access-Guid。
	Guid string `json:"-"`
	// LoginType 登录渠道（WX / QC），refresh 请求体需要；空按 WX 处理。
	LoginType string `json:"-"`
	// UID 上游账号标识（可空；仅透出观测）。
	UID string `json:"-"`
	// Note 备注。
	Note string `json:"-"`
	// CreatedAt RFC3339，首次落盘时间。
	CreatedAt string `json:"-"`
	// Disabled 为真时不参与自动分配。
	Disabled bool `json:"-"`
}

// authFile 落盘形态（对齐 workbuddy2api 的 auths 约定：嵌套 auth/account 两段，
// camelCase 字段）。扁平形态（顶层 token / refresh_token）亦兼容读取。
type authFile struct {
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken,omitempty"`
		ExpiresAt    int64  `json:"expiresAt,omitempty"`
		BaseURL      string `json:"baseURL,omitempty"`
	} `json:"auth"`
	Account struct {
		UID      string `json:"uid,omitempty"`
		Nickname string `json:"nickname,omitempty"`
	} `json:"account"`
	LoginType string `json:"loginType,omitempty"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Guid      string `json:"guid,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
}

// MarshalJSON 嵌套形态落盘（锁内取值，防与 refresh 写回撕裂）。
func (a *Auth) MarshalJSON() ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var f authFile
	f.Auth.AccessToken = a.Token
	f.Auth.RefreshToken = a.RefreshToken
	f.Auth.ExpiresAt = a.ExpiresAt
	f.Auth.BaseURL = a.BaseURL
	f.Account.UID = a.UID
	f.Account.Nickname = a.Name
	f.LoginType = a.LoginType
	f.Note = a.Note
	f.CreatedAt = a.CreatedAt
	f.Guid = a.Guid
	f.Disabled = a.Disabled
	return json.Marshal(&f)
}

// UnmarshalJSON 解析落盘文件：嵌套形态（workbuddy2api 约定）为主，兼容扁平形态。
func (a *Auth) UnmarshalJSON(b []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var f authFile
	if err := json.Unmarshal(b, &f); err == nil && f.Auth.AccessToken != "" {
		a.Token = f.Auth.AccessToken
		a.RefreshToken = f.Auth.RefreshToken
		a.ExpiresAt = f.Auth.ExpiresAt
		a.BaseURL = f.Auth.BaseURL
		a.UID = f.Account.UID
		a.Name = f.Account.Nickname
		a.LoginType = f.LoginType
		a.Note = f.Note
		a.CreatedAt = f.CreatedAt
		a.Guid = f.Guid
		a.Disabled = f.Disabled
		return nil
	}
	var flat struct {
		Name         string `json:"name"`
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    int64  `json:"expires_at"`
		BaseURL      string `json:"base_url"`
		UID          string `json:"uid"`
		LoginType    string `json:"login_type"`
		Note         string `json:"note"`
		CreatedAt    string `json:"created_at"`
		Guid         string `json:"guid"`
		Disabled     bool   `json:"disabled"`
	}
	if err := json.Unmarshal(b, &flat); err != nil {
		return err
	}
	a.Name = flat.Name
	a.Token = flat.Token
	a.RefreshToken = flat.RefreshToken
	a.ExpiresAt = flat.ExpiresAt
	a.BaseURL = flat.BaseURL
	a.UID = flat.UID
	a.LoginType = flat.LoginType
	a.Note = flat.Note
	a.CreatedAt = flat.CreatedAt
	a.Guid = flat.Guid
	a.Disabled = flat.Disabled
	return nil
}

// Clone 返回字段的逐项拷贝（不拷贝锁）。拷贝在 a.mu 内完成，保证与并发
// refresh 写回不撕裂。
func (a *Auth) Clone() *Auth {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &Auth{
		Name:         a.Name,
		Token:        a.Token,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		BaseURL:      a.BaseURL,
		UID:          a.UID,
		LoginType:    a.LoginType,
		Note:         a.Note,
		CreatedAt:    a.CreatedAt,
		Guid:         a.Guid,
		Disabled:     a.Disabled,
	}
}

// TokenValue 加锁读取 access token 快照。
func (a *Auth) TokenValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Token
}

// RefreshTokenValue 加锁读取 refresh token 快照。
func (a *Auth) RefreshTokenValue() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// WriteBack 加锁写回刷新后的 token（空值保留旧值）。
func (a *Auth) WriteBack(token, refreshToken string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if token != "" {
		a.Token = token
	}
	if refreshToken != "" {
		a.RefreshToken = refreshToken
	}
}

// SetExpiresAt 写入 access token 过期时刻（Unix 秒）。
func (a *Auth) SetExpiresAt(unix int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ExpiresAt = unix
}

// Dormant 为真时不参与自动分配。
func (a *Auth) Dormant() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Disabled
}

// SetDormant 设置是否休眠。
func (a *Auth) SetDormant(v bool) {
	a.mu.Lock()
	a.Disabled = v
	a.mu.Unlock()
}

// SetName 写入展示名。
func (a *Auth) SetName(name string) {
	a.mu.Lock()
	a.Name = strings.TrimSpace(name)
	a.mu.Unlock()
}

// ExpiresAtValue 返回 access token 过期时刻（Unix 秒；0 = 未知）。
func (a *Auth) ExpiresAtValue() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ExpiresAt
}

// ID 账号在多账号目录里的键。优先 openid，否则 guid。
func (a *Auth) ID() string {
	if a == nil {
		return ""
	}
	if s := strings.TrimSpace(a.UID); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.Guid); s != "" {
		return "g-" + s
	}
	return ""
}

// Label 日志与面板用的账号标签（优先昵称，其次 UID，都没有则 marvis）。
func (a *Auth) Label() string {
	if a == nil {
		return "-"
	}
	if s := strings.TrimSpace(a.Name); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.UID); s != "" {
		return s
	}
	return "marvis"
}

// Validate 校验字段；返回人读错误（面板直接展示）。
func (a *Auth) Validate() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Token = strings.TrimSpace(a.Token)
	a.RefreshToken = strings.TrimSpace(a.RefreshToken)
	if a.Token == "" {
		return errors.New("token 不能为空（从 Marvis 客户端获取后粘贴到这里）")
	}
	if a.BaseURL != "" {
		a.BaseURL = strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
		if !strings.HasPrefix(a.BaseURL, "http://") && !strings.HasPrefix(a.BaseURL, "https://") {
			return fmt.Errorf("base_url 必须以 http:// 或 https:// 开头（当前 %q）", a.BaseURL)
		}
	}
	a.Name = strings.TrimSpace(a.Name)
	a.Note = strings.TrimSpace(a.Note)
	a.Guid = strings.TrimSpace(a.Guid)
	a.UID = strings.TrimSpace(a.UID)
	a.LoginType = strings.ToUpper(strings.TrimSpace(a.LoginType))
	if a.LoginType == "" {
		a.LoginType = "WX"
	}
	if a.LoginType != "WX" && a.LoginType != "QC" {
		return fmt.Errorf("loginType 仅支持 WX / QC（当前 %q）", a.LoginType)
	}
	return nil
}

// File 单个凭证文件的读写点（并发安全）。
type File struct {
	path string
	mu   sync.Mutex // 序列化落盘，避免 tmp 文件名互撞
}

// NewFile 构建凭证文件存储（目录不存在时 Save 自动创建）。
func NewFile(path string) *File { return &File{path: path} }

// Path 返回凭证文件路径（启动日志透出）。
func (f *File) Path() string { return f.path }

// Load 读取凭证。文件不存在返回 (nil, nil)——未配置凭证是合法初始状态。
func (f *File) Load() (*Auth, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", f.path, err)
	}
	var a Auth
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("parse %s: %w", f.path, err)
	}
	if strings.TrimSpace(a.Token) == "" {
		return nil, fmt.Errorf("%s: token 为空", f.path)
	}
	return &a, nil
}

// Save 原子落盘凭证（0600，tmp + rename）。
func (f *File) Save(a *Auth) error {
	if err := a.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(f.path), err)
	}
	if a.CreatedAt == "" {
		a.CreatedAt = time.Now().Format(time.RFC3339)
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(f.path, raw)
}

// Delete 删除凭证文件；不存在视为成功（幂等）。
func (f *File) Delete() error {
	if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Exists 凭证文件是否存在。
func (f *File) Exists() bool {
	_, err := os.Stat(f.path)
	return err == nil
}

// Stat 返回文件的修改时刻与大小（热加载比对指纹用）。文件不存在时返回 os.ErrNotExist。
func (f *File) Stat() (mod time.Time, size int64, err error) {
	fi, err := os.Stat(f.path)
	if err != nil {
		return time.Time{}, 0, err
	}
	return fi.ModTime(), fi.Size(), nil
}

// atomicWrite tmp + rename 原子替换（权限 0600）。
// 单文件 Docker bind mount 无法 rename 覆盖挂载点（Linux EBUSY）：
// 回退为原地写入（非原子，功能不受影响）。
func atomicWrite(fp string, raw []byte) error {
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, fp); err != nil {
		if !errors.Is(err, syscallEBUSY) {
			_ = os.Remove(tmp)
			return err
		}
		// bind mount 回退：原地写。
		if werr := writeFileDirect(fp, raw); werr != nil {
			_ = os.Remove(tmp)
			return werr
		}
		_ = os.Remove(tmp)
		return nil
	}
	return nil
}
