package kernel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
)

// Instance 一个账号的官方 Host + Agent。进程之间用独立目录和套接字隔开。
type Instance struct {
	id   string
	auth *account.Auth
	root string

	mu       sync.Mutex
	ready    chan struct{}
	readyOK  bool
	startErr error
	stopped  bool

	runMu sync.Mutex

	hostCmd   *exec.Cmd
	agentCmd  *exec.Cmd
	hostIn    io.WriteCloser
	agentIn   io.WriteCloser
	br        *bridge
	agentPort int

	agentSock string
	kbSock    string
	port      int
	token     string
}

func newInstance(id string, auth *account.Auth, root string) *Instance {
	return &Instance{
		id:    id,
		auth:  auth.Clone(),
		root:  root,
		ready: make(chan struct{}),
	}
}

func (in *Instance) ID() string { return in.id }

func (in *Instance) Auth() *account.Auth { return in.auth }

func (in *Instance) Ready() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.readyOK && !in.stopped
}

func (in *Instance) Err() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.startErr
}

func (in *Instance) Dead() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.stopped {
		return true
	}
	if in.hostCmd == nil || in.hostCmd.Process == nil {
		return false
	}
	return syscall.Kill(in.hostCmd.Process.Pid, 0) != nil
}

func (in *Instance) WaitReady(ctx context.Context) error {
	select {
	case <-in.ready:
		in.mu.Lock()
		err := in.startErr
		ok := in.readyOK
		in.mu.Unlock()
		if err != nil {
			return err
		}
		if !ok {
			return errNotReady
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (in *Instance) sameToken(a *account.Auth) bool {
	return a != nil && a.TokenValue() == in.auth.TokenValue() && a.Guid == in.auth.Guid
}

func (in *Instance) start(cacheDir, dylib string) {
	defer close(in.ready)
	if err := in.launch(cacheDir, dylib); err != nil {
		in.fail(err)
		in.stop()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := in.waitReady(ctx); err != nil {
		in.fail(err)
		in.stop()
		return
	}
	in.mu.Lock()
	in.readyOK = true
	in.mu.Unlock()
	log.Printf("[kernel] 账号 %s 内核已就绪 port=%d", in.id, in.port)
}

func (in *Instance) fail(err error) {
	in.mu.Lock()
	in.startErr = err
	in.mu.Unlock()
	log.Printf("[kernel] 账号 %s 启动失败: %v", in.id, err)
}

func (in *Instance) launch(cacheDir, dylib string) error {
	if _, err := os.Stat(hostBin()); err != nil {
		return errNoInstall
	}
	if _, err := os.Stat(agentBin()); err != nil {
		return fmt.Errorf("未找到 MarvisAgent")
	}
	suffix, err := randSuffix()
	if err != nil {
		return err
	}
	for _, sub := range []string{"home", "tmp", "data", "logs", "agent-log", "agent-db"} {
		if err := os.MkdirAll(filepath.Join(in.root, sub), 0o700); err != nil {
			return err
		}
	}
	in.agentSock = "/tmp/marvisagent_" + suffix
	in.kbSock = "/tmp/marvisgateway_kb_" + suffix
	agentPort, err := freePort()
	if err != nil {
		return err
	}
	hostPort, err := freePort()
	if err != nil {
		return err
	}
	guid := strings.TrimSpace(in.auth.Guid)
	if guid == "" {
		return fmt.Errorf("账号缺少 guid")
	}
	loginType := in.auth.LoginType
	if loginType == "" {
		loginType = "WX"
	}
	exp := in.auth.ExpiresAt
	if exp == 0 {
		exp = time.Now().Add(2 * time.Hour).Unix()
	}
	login := map[string]any{
		"isLoggedIn": true, "action": "login",
		"openId": in.auth.UID, "loginType": loginType,
		"accessToken": in.auth.TokenValue(), "refreshToken": in.auth.RefreshTokenValue(),
		"guid": guid, "qimei36": guid, "expireTime": exp,
	}
	sock := filepath.Join(in.root, "gw.sock")
	br, err := newBridge(sock, guid, login)
	if err != nil {
		return err
	}
	in.br = br

	agent := exec.Command(agentBin(),
		"--port_file", filepath.Join(in.root, "agent_port.ini"),
		"--log_dir", filepath.Join(in.root, "agent-log"),
		"--home_dir", filepath.Join(in.root, "data"),
		"--work_mode", "cloud",
		"--db_dir", filepath.Join(in.root, "agent-db"),
		"--ipc_addr", "ipc://"+in.agentSock,
		"--user_id", safeID(in.id),
		"--port", fmt.Sprint(agentPort),
	)
	agent.Env = []string{
		"HOME=" + filepath.Join(in.root, "home"),
		"TMPDIR=" + filepath.Join(in.root, "tmp"),
		"PATH=/usr/bin:/bin",
		"USER=" + userName(),
		"LOGNAME=" + userName(),
		"MARVIS_IPC_ADDR=ipc://" + in.agentSock,
	}
	agent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	agentErr, _ := os.OpenFile(filepath.Join(in.root, "agent.stderr"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	agent.Stderr = agentErr
	agent.Stdout = io.Discard
	ain, err := agent.StdinPipe()
	if err != nil {
		return err
	}
	if err := agent.Start(); err != nil {
		return fmt.Errorf("启动 Agent 失败: %w", err)
	}
	in.agentCmd = agent
	in.agentIn = ain
	in.agentPort = agentPort
	if err := waitFile(in.agentSock, 10*time.Second); err != nil {
		return fmt.Errorf("Agent 没有建起 IPC: %w", err)
	}

	profile := filepath.Join(in.root, "sandbox.sb")
	if err := os.WriteFile(profile, []byte(sandboxProfile), 0o600); err != nil {
		return err
	}
	hookLog := filepath.Join(in.root, "hook.log")
	args := []string{"-f", profile, "/usr/bin/env",
		"HOME=" + filepath.Join(in.root, "home"),
		"TMPDIR=" + filepath.Join(in.root, "tmp"),
		"PATH=/usr/bin:/bin",
		"USER=" + userName(),
		"LOGNAME=" + userName(),
		"IPC_PIPE_ADDRESS=ipc://" + sock,
		"DYLD_INSERT_LIBRARIES=" + dylib,
		"MARVIS_AGENT_IPC_PATH=" + in.agentSock,
		"MARVIS_KB_IPC_PATH=" + in.kbSock,
		"MARVIS_IPC_HOOK_LOG=" + hookLog,
		hostBin(), "start",
		"--home-dir", filepath.Join(in.root, "data"),
		"--log-dir", filepath.Join(in.root, "logs"),
		"--port", fmt.Sprint(hostPort),
		"--guid", guid,
		"--user-id", safeID(in.id),
	}
	if b := beaconLib(); b != "" {
		args = append(args, "--beacon-lib", b)
	}
	host := exec.Command("sandbox-exec", args...)
	host.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	hostErr, _ := os.OpenFile(filepath.Join(in.root, "host.stderr"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	host.Stderr = hostErr
	host.Stdout = io.Discard
	hin, err := host.StdinPipe()
	if err != nil {
		return err
	}
	if err := host.Start(); err != nil {
		return fmt.Errorf("启动 Host 失败: %w", err)
	}
	in.hostCmd = host
	in.hostIn = hin
	if err := waitHook(hookLog, 3*time.Second, host); err != nil {
		return err
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(host.Process.Pid, 0) != nil {
			return fmt.Errorf("Host 提前退出")
		}
		tok, port, ok := br.Token()
		if ok {
			in.token = tok
			in.port = port
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("Host 没有连上登录桥")
}

func (in *Instance) waitReady(ctx context.Context) error {
	time.Sleep(1200 * time.Millisecond) // 等 WSS 插件加载完再推登录
	if in.br == nil {
		return errNoBridge
	}
	if err := in.br.Push("agent", "onLaunch", map[string]any{"port": in.agentPort}); err != nil {
		return err
	}
	if err := in.br.Push("account", "onLoginStateChanged", in.br.login); err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	var readyAt time.Time
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		ws, err := dialWS(ctx, in.wsURL())
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		msg, err := ws.ReadText(ctx)
		ws.Close()
		if err == nil && strings.Contains(msg, `"agentConnectionStatus":"Ready"`) {
			if readyAt.IsZero() {
				readyAt = time.Now()
			}
			if in.schemaReady() || time.Since(readyAt) > 2*time.Second {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("内核在 20 秒内没有进入可用状态")
}

func (in *Instance) schemaReady() bool {
	uid := strings.TrimSpace(in.auth.UID)
	if uid == "" {
		return false
	}
	p := filepath.Join(in.root, "data", "User", uid, "database", "data.db")
	b, err := os.ReadFile(p)
	return err == nil && strings.Contains(string(b), "CREATE TABLE conversations")
}

func (in *Instance) wsURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/?token=%s", in.port, in.token)
}

// Run 发一条消息，把文本增量写进 ch。调用方必须读到通道关闭。
func (in *Instance) Run(ctx context.Context, prompt string) (<-chan Delta, error) {
	if err := in.WaitReady(ctx); err != nil {
		return nil, err
	}
	in.runMu.Lock()
	ch := make(chan Delta, 32)
	go func() {
		defer in.runMu.Unlock()
		defer close(ch)
		in.stream(ctx, prompt, ch)
	}()
	return ch, nil
}

func (in *Instance) stream(ctx context.Context, prompt string, ch chan<- Delta) {
	ws, err := dialWS(ctx, in.wsURL())
	if err != nil {
		ch <- Delta{Err: fmt.Errorf("连接内核失败: %w", err)}
		return
	}
	defer ws.Close()
	first, err := ws.ReadText(ctx)
	if err != nil {
		ch <- Delta{Err: err}
		return
	}
	if d := parseHostEvent(first); d.Err != nil {
		ch <- d
		return
	}
	reqID := newID()
	body, _ := json.Marshal(map[string]any{
		"event": "agent.run", "requestId": reqID,
		"payload": map[string]any{
			"conversation_id": newID(), "message": prompt, "response_id": newID(),
			"attachments": []any{}, "mcp_manage_tools_enabled": false,
		},
	})
	if err := ws.WriteText(string(body)); err != nil {
		ch <- Delta{Err: err}
		return
	}
	for {
		msg, err := ws.ReadText(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			ch <- Delta{Err: err}
			return
		}
		d := parseHostEvent(msg)
		if d.Text == "" && d.Err == nil && !d.Done && d.Prompt == 0 && d.Compl == 0 {
			continue
		}
		ch <- d
		if d.Err != nil || d.Done {
			return
		}
	}
}

func (in *Instance) stop() {
	in.mu.Lock()
	if in.stopped {
		in.mu.Unlock()
		return
	}
	in.stopped = true
	in.readyOK = false
	host, agent := in.hostCmd, in.agentCmd
	hin, ain := in.hostIn, in.agentIn
	br := in.br
	socks := []string{in.agentSock, in.kbSock}
	in.mu.Unlock()
	if hin != nil {
		_ = hin.Close()
	}
	if ain != nil {
		_ = ain.Close()
	}
	killCmd(host)
	killCmd(agent)
	if br != nil {
		br.Close()
	}
	for _, p := range socks {
		if strings.HasPrefix(p, "/tmp/marvisagent_") || strings.HasPrefix(p, "/tmp/marvisgateway_kb_") {
			_ = os.Remove(p)
		}
	}
}

func killCmd(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func randSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitFile(path string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("超时")
}

func waitHook(path string, d time.Duration, cmd *exec.Cmd) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cmd.Process != nil && syscall.Kill(cmd.Process.Pid, 0) != nil {
			return fmt.Errorf("Host 提前退出")
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), "loaded") {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("IPC 重定向没有加载，已停止，避免碰到正在运行的 Marvis")
}

func userName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "user"
}

func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		return "account"
	}
	if len(s) > 32 {
		return s[:32]
	}
	return s
}
