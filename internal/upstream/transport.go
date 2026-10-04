// 共享传输层：单例硬化 Transport，chat 与短 RPC 共用。
//
// 对齐 workbuddy2api 的 transport.go 口径：
//   - 显式拨号/握手超时（默认 Transport 无 dial 超时，靠运气）；
//   - 空 TLSNextProto 是关闭 h2 的唯一正确方式（h2 对长 SSE 有流控相互干扰）；
//   - 连接池参数调优（空闲连接复用，降低冷启动 TTFB）；
//   - ResponseHeaderTimeout 兜底 chat 首字节（Client.HeaderTimeout 注入）。
package upstream

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// newTransport 构建硬化 Transport。headerTimeout 为响应头超时（chat 首字节上限）。
func newTransport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		// 关闭 HTTP/2：空非 nil TLSNextProto。
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: headerTimeout,
	}
}
