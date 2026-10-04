// 前端资源 embed 与静态服务：index.html + app.js 编译进二进制。
package panel

import (
	"embed"
	"net/http"
)

//go:embed index.html app.js
var webFS embed.FS

// index 面板页（无秘密，可匿名加载；密钥只在 /panel/api/* 鉴权时使用）。
// no-cache：单文件面板随二进制更新，浏览器不得用旧页面配新接口。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	raw, err := webFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "panel assets missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

// appScript 面板脚本（no-cache，同 index：避免升级后浏览器跑旧 JS）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	raw, err := webFS.ReadFile("app.js")
	if err != nil {
		http.Error(w, "panel assets missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

// setSecurityHeaders 面板与 API 共用的安全响应头。
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	// 自持单页：脚本仅本源；样式内联（单文件面板的约定）；不加载任何外部资源。
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'")
}
