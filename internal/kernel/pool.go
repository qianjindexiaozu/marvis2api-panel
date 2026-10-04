package kernel

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
)

// Refresher 启动前刷新即将过期的 token。失败不阻止启动。
type Refresher func(ctx context.Context, a *account.Auth) error

// Pool 按账号各持有一份内核。
type Pool struct {
	root    string
	mu      sync.Mutex
	inst    map[string]*Instance
	active  string
	dylib   string
	refresh Refresher
}

// New 创建池。root 是每账号数据的父目录。
func New(root string) *Pool {
	return &Pool{root: root, inst: map[string]*Instance{}}
}

// Installed 本机能否启动官方内核。
func Installed() bool { return installed() }

func (p *Pool) SetRefresher(fn Refresher) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.refresh = fn
	p.mu.Unlock()
}

func (p *Pool) SetActive(id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.active = id
	p.mu.Unlock()
}

func (p *Pool) Active() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// Ensure 后台启动或在凭证变化后重启。重复调用同一凭证不会重启。
func (p *Pool) Ensure(a *account.Auth) {
	if p == nil || a == nil || !installed() {
		return
	}
	go p.ensure(a.Clone())
}

func (p *Pool) ensure(a *account.Auth) {
	id := a.ID()
	if id == "" {
		return
	}
	if p.refresh != nil && a.ExpiresAt > 0 && a.ExpiresAt < time.Now().Add(10*time.Minute).Unix() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = p.refresh(ctx, a)
		cancel()
	}
	dylib, err := p.redirect()
	if err != nil {
		log.Printf("[kernel] %v", err)
		return
	}
	p.mu.Lock()
	if old := p.inst[id]; old != nil && old.sameToken(a) && !old.Dead() {
		p.mu.Unlock()
		return
	}
	old := p.inst[id]
	in := newInstance(id, a, filepath.Join(p.root, safeID(id)))
	p.inst[id] = in
	p.mu.Unlock()
	if old != nil {
		old.stop()
	}
	in.start(p.root, dylib)
}

func (p *Pool) redirect() (string, error) {
	p.mu.Lock()
	if p.dylib != "" {
		d := p.dylib
		p.mu.Unlock()
		return d, nil
	}
	p.mu.Unlock()
	d, err := compileRedirect(p.root)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	p.dylib = d
	p.mu.Unlock()
	return d, nil
}

// Pick 按请求头或当前选中账号取出已就绪的内核。
func (p *Pool) Pick(hint string) (*Instance, error) {
	if p == nil || !installed() {
		return nil, errNoInstall
	}
	p.mu.Lock()
	in, err := p.pickLocked(hint)
	var restart *account.Auth
	if in != nil && in.Dead() {
		restart = in.Auth()
		in = nil
		err = fmt.Errorf("账号内核已退出，正在重新启动，请稍后再试")
	}
	p.mu.Unlock()
	if restart != nil {
		p.Ensure(restart)
	}
	return in, err
}

func (p *Pool) pickLocked(hint string) (*Instance, error) {
	if len(p.inst) == 0 {
		return nil, errNoAccount
	}
	var in *Instance
	if hint != "" {
		in = p.matchLocked(hint)
		if in == nil {
			return nil, errNoAccount
		}
	} else if p.active != "" {
		in = p.inst[p.active]
	} else if len(p.inst) == 1 {
		for _, v := range p.inst {
			in = v
		}
	}
	if in == nil {
		return nil, errNoAccount
	}
	return in, nil
}

func (p *Pool) matchLocked(hint string) *Instance {
	if in := p.inst[hint]; in != nil {
		return in
	}
	for _, in := range p.inst {
		a := in.Auth()
		if a == nil {
			continue
		}
		if a.ID() == hint || a.UID == hint || a.Label() == hint || a.Name == hint {
			return in
		}
	}
	return nil
}

// View 面板用的状态，不含 token。
type View struct {
	ID     string `json:"id"`
	Kernel string `json:"kernel"`
	Error  string `json:"error,omitempty"`
}

func (p *Pool) View(id string) View {
	v := View{ID: id, Kernel: "stopped"}
	if p == nil {
		return v
	}
	if !installed() {
		v.Kernel = "unsupported"
		return v
	}
	p.mu.Lock()
	in := p.inst[id]
	p.mu.Unlock()
	if in == nil {
		return v
	}
	select {
	case <-in.ready:
		if in.Ready() {
			v.Kernel = "ready"
			return v
		}
		if err := in.Err(); err != nil {
			v.Kernel = "error"
			v.Error = err.Error()
			return v
		}
		v.Kernel = "stopped"
		return v
	default:
		v.Kernel = "starting"
		return v
	}
}

// Remove 停掉并忘掉一个账号的内核。
func (p *Pool) Remove(id string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	in := p.inst[id]
	delete(p.inst, id)
	if p.active == id {
		p.active = ""
	}
	p.mu.Unlock()
	if in != nil {
		in.stop()
	}
}

// Close 停掉全部内核。
func (p *Pool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	all := make([]*Instance, 0, len(p.inst))
	for _, in := range p.inst {
		all = append(all, in)
	}
	p.inst = map[string]*Instance{}
	p.mu.Unlock()
	for _, in := range all {
		in.stop()
	}
}
