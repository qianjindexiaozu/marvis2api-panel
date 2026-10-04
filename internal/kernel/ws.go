package kernel

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

type wsConn struct {
	c net.Conn
	r *bufio.Reader
}

func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		conn.Close()
		return nil, err
	}
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, base64.StdEncoding.EncodeToString(key))
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		conn.Close()
		return nil, fmt.Errorf("websocket 握手失败: %s", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &wsConn{c: conn, r: br}, nil
}

func (c *wsConn) Close() error { return c.c.Close() }

func (c *wsConn) WriteText(s string) error {
	return writeFrame(c.c, 0x1, []byte(s))
}

func (c *wsConn) ReadText(ctx context.Context) (string, error) {
	var acc []byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		idle := time.Now().Add(3 * time.Minute)
		if dl, ok := ctx.Deadline(); ok && dl.Before(idle) {
			idle = dl
		}
		_ = c.c.SetReadDeadline(idle)
		fin, op, payload, err := readFrame(c.r)
		if err != nil {
			return "", err
		}
		switch op {
		case 0x9: // ping
			_ = writeFrame(c.c, 0xA, payload)
			continue
		case 0x8:
			return "", io.EOF
		case 0x1, 0x0:
			acc = append(acc, payload...)
			if fin && (op == 0x1 || len(acc) > 0) {
				return string(acc), nil
			}
		default:
			if fin && op == 0x1 {
				return string(payload), nil
			}
		}
	}
}

func writeFrame(w io.Writer, op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(0x80|n))
	case n <= 65535:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		hdr = append(hdr, ext[:]...)
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

func readFrame(r *bufio.Reader) (fin bool, op byte, payload []byte, err error) {
	b0, err := r.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	b1, err := r.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	fin = b0&0x80 != 0
	op = b0 & 0x0f
	masked := b1&0x80 != 0
	ln := uint64(b1 & 0x7f)
	switch ln {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		ln = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return false, 0, nil, err
		}
		ln = binary.BigEndian.Uint64(ext[:])
	}
	if ln > 8<<20 {
		return false, 0, nil, fmt.Errorf("websocket 帧过大")
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(r, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, ln)
	if _, err = io.ReadFull(r, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}
