package kernel

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sync"
)

// bridge 扮演官方客户端：Host 连上来问登录信息，这里按这个账号回答。
type bridge struct {
	ln   net.Listener
	mu   sync.Mutex
	conn net.Conn
	wmu  sync.Mutex

	token string
	port  int
	login map[string]any
	guid  string
	stop  chan struct{}
}

func newBridge(sock, guid string, login map[string]any) (*bridge, error) {
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(sock, 0o600)
	b := &bridge{ln: ln, login: login, guid: guid, stop: make(chan struct{})}
	go b.accept()
	return b, nil
}

func (b *bridge) Close() {
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	_ = b.ln.Close()
	b.mu.Lock()
	if b.conn != nil {
		_ = b.conn.Close()
	}
	b.mu.Unlock()
}

func (b *bridge) Token() (string, int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.token, b.port, b.token != ""
}

func (b *bridge) accept() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		b.mu.Lock()
		if b.conn != nil {
			_ = b.conn.Close()
		}
		b.conn = conn
		b.mu.Unlock()
		go b.read(conn)
	}
}

func (b *bridge) read(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg struct {
			Namespace  string          `json:"namespace"`
			Method     string          `json:"method"`
			CallbackID string          `json:"callbackId"`
			Params     json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.CallbackID == "" {
			continue
		}
		params := map[string]any{"code": 0}
		switch msg.Namespace + "." + msg.Method {
		case "base.init":
			var p struct {
				Token string `json:"token"`
				Port  int    `json:"port"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			b.mu.Lock()
			if p.Token != "" {
				b.token = p.Token
			}
			if p.Port != 0 {
				b.port = p.Port
			}
			b.mu.Unlock()
			params = map[string]any{"guid": b.guid, "client_version": "1.0.0.10632"}
		case "account.getLoginInfo":
			params = b.login
		case "location.getAuthStatus":
			params = map[string]any{"code": 0, "data": map[string]any{"status": "authorized"}}
		case "location.getCoordinate":
			params = map[string]any{"code": 0, "data": map[string]any{"latitude": 31.23, "longitude": 121.47}}
		}
		_ = b.write(conn, map[string]any{
			"type": "ack", "protocalVersion": "1.0",
			"callbackId": msg.CallbackID, "namespace": msg.Namespace, "method": msg.Method,
			"params": params,
		})
	}
}

func (b *bridge) Push(namespace, method string, params any) error {
	b.mu.Lock()
	conn := b.conn
	b.mu.Unlock()
	if conn == nil {
		return errNoBridge
	}
	return b.write(conn, map[string]any{
		"type": "send", "protocalVersion": "1.0",
		"callbackId": "notify-" + method, "namespace": namespace, "method": method,
		"params": params,
	})
}

func (b *bridge) write(conn net.Conn, obj any) error {
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_, err = conn.Write(raw)
	return err
}
