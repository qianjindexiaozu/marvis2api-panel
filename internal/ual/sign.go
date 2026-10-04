// Package ual implements upstream signing with a caller-supplied key.
package ual

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

var ErrNoAccessKey = errors.New("未加载上游签名钥匙，请先运行 bash scripts/prepare.sh")

// Sign follows the upstream protocol: md5(body + timestamp + accessKey + nonce).
// It does not change body or contain a default key.
func Sign(body []byte, timestamp, nonce, accessKey string) (string, error) {
	if strings.TrimSpace(accessKey) == "" {
		return "", ErrNoAccessKey
	}
	h := md5.New()
	_, _ = h.Write(body)
	_, _ = io.WriteString(h, timestamp)
	_, _ = io.WriteString(h, accessKey)
	_, _ = io.WriteString(h, nonce)
	return hex.EncodeToString(h.Sum(nil)), nil
}
