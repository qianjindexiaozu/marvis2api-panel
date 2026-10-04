package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/apikey"
)

func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"password_split": p.cfg.Passwords != nil && p.cfg.Passwords.Get() != "",
		"keys":           p.publicKeys(),
	})
}

func (p *Panel) publicKeys() []map[string]any {
	if p.cfg.Keys == nil {
		return []map[string]any{}
	}
	var out []map[string]any
	for _, k := range p.cfg.Keys.List() {
		if k.ID == "legacy" {
			continue
		}
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "mask": apikey.Mask(k.Secret),
			"created_at": k.CreatedAt, "last_used": k.LastUsed,
			"last_check": k.LastCheck, "last_ok": k.LastOK, "disabled": k.Disabled,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func (p *Panel) keysReveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Config bool   `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Config {
		writeErr(w, http.StatusBadRequest, "配置文件里的密钥已停用")
		return
	}
	secret := ""
	if p.cfg.Keys != nil {
		k, ok := p.cfg.Keys.Get(req.ID)
		if !ok {
			writeErr(w, http.StatusNotFound, "没有这把密钥")
			return
		}
		secret = k.Secret
	}
	if secret == "" {
		writeErr(w, http.StatusNotFound, "没有可复制的密钥")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "secret": secret})
}

func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "未启用密钥存储")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req)
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "请填写密钥名称")
		return
	}
	k, err := p.cfg.Keys.Create(req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": k.ID, "name": k.Name, "secret": k.Secret,
	})
}

func (p *Panel) keysDisable(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "未启用密钥存储")
		return
	}
	var req struct {
		ID       string `json:"id"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, http.StatusBadRequest, "缺少密钥 id")
		return
	}
	if !p.cfg.Keys.SetDisabled(req.ID, req.Disabled) {
		writeErr(w, http.StatusNotFound, "没有这把密钥")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) keysDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "未启用密钥存储")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" || id == "legacy" {
		writeErr(w, http.StatusBadRequest, "不能删除这把密钥")
		return
	}
	if !p.cfg.Keys.Delete(id) {
		writeErr(w, http.StatusNotFound, "没有这把密钥")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) keysCheck(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Gateway == nil {
		writeErr(w, http.StatusNotImplemented, "网关未注入，无法在线校验")
		return
	}
	var req struct {
		ID     string `json:"id"`
		Config bool   `json:"config"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Config {
		writeErr(w, http.StatusBadRequest, "配置文件里的密钥已停用")
		return
	}
	secret := ""
	if p.cfg.Keys != nil {
		k, ok := p.cfg.Keys.Get(req.ID)
		if !ok {
			writeErr(w, http.StatusNotFound, "没有这把密钥")
			return
		}
		secret = k.Secret
	}
	if secret == "" {
		writeErr(w, http.StatusBadRequest, "没有可校验的密钥")
		return
	}
	rr := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rr.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	p.cfg.Gateway.ServeHTTP(rec, rr)
	ok := rec.Code == http.StatusOK
	if p.cfg.Keys != nil && req.ID != "" {
		p.cfg.Keys.NoteCheck(req.ID, ok)
	}
	msg := ""
	if !ok {
		msg = strings.TrimSpace(rec.Body.String())
		if len(msg) > 180 {
			msg = msg[:180]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "status": rec.Code, "error": msg})
}

func (p *Panel) passwordSet(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Passwords == nil {
		writeErr(w, http.StatusNotImplemented, "未启用独立登录密码")
		return
	}
	var req struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Current != p.loginPassword() {
		writeErr(w, http.StatusUnauthorized, "当前密码不对")
		return
	}
	next := strings.TrimSpace(req.Password)
	if len(next) < 4 {
		writeErr(w, http.StatusBadRequest, "新密码至少 4 位")
		return
	}
	if err := p.cfg.Passwords.Set(next); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.sess.RevokeAll()
	if token, ok, _ := p.sess.Create(next, next, clientSource(r)); ok {
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(p.sess.ttl.Seconds()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
