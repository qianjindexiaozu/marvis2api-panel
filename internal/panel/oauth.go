// 微信扫码授权（无 App 登录）的面板端点。
//
// 流程：前端 POST start 拿到二维码（data URL）→ 弹窗展示并循环 GET poll
// （服务端对微信 lp 端点长轮询，单次最长约 25s）→ 用户手机确认后服务端
// 自动完成落地换证并直接落盘凭证 → poll 返回 done，前端刷新总览。
package panel

import (
	"encoding/base64"
	"log"
	"net/http"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/oauth"
)

// oauthWeChatStart POST /panel/api/login/wechat/start
// 每次扫码用新的 guid。账号之间不共用设备号，避免后一次登录把前一个挤下线。
func (p *Panel) oauthWeChatStart(w http.ResponseWriter, r *http.Request) {
	sess, err := p.wxOAuth.Start(r.Context(), "")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "发起扫码授权失败: "+err.Error())
		return
	}
	log.Printf("[panel] wechat oauth start session=%s guid=%s", sess.ID[:8], sess.GUID[:8])
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"qr":         "data:image/png;base64," + base64.StdEncoding.EncodeToString(sess.QR),
		"expires_in": int((6 * time.Minute).Seconds()),
	})
}

// oauthWeChatPoll GET /panel/api/login/wechat/poll?id=<session_id>
// 推进一次长轮询并返回当前状态；done 时凭证已落盘并生效。
func (p *Panel) oauthWeChatPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少会话 id")
		return
	}
	status, err := p.wxOAuth.Poll(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	resp := map[string]any{"status": status}
	if errText := oauthErrText(p.wxOAuth, id); errText != "" {
		resp["error"] = errText
	}
	if status != oauth.StatusDone {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	cred := p.wxOAuth.Credential(id)
	if cred == nil {
		resp["status"] = "error"
		resp["error"] = "会话凭证丢失，请重新扫码"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	revived, err := p.applyCredential(cred)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp["revived"] = revived
	resp["account"] = p.cfg.State.Status()
	log.Printf("[panel] wechat oauth login ok openid=%s… login_type=%s（无 App 登录完成）", cred.OpenID[:8], cred.LoginType)
	writeJSON(w, http.StatusOK, resp)
}

// applyCredential 把扫码凭证写入运行状态并落盘（OAuth 登录的核心落点）。
func (p *Panel) applyCredential(cred *oauth.Credential) (bool, error) {
	a := &account.Auth{
		Name:         cred.Nickname,
		Token:        cred.AccessToken,
		RefreshToken: cred.RefreshToken,
		UID:          cred.OpenID,
		Guid:         cred.GUID,
		LoginType:    oauth.NormalizeLoginType(cred.LoginType),
	}
	if cred.ExpiresIn > 0 {
		a.SetExpiresAt(time.Now().Unix() + cred.ExpiresIn)
	}
	changed, err := p.adopt(a)
	if err != nil {
		return false, err
	}
	p.cfg.State.Revive()
	p.cfg.State.Flush()
	return changed, nil
}

// oauthQQStart POST /panel/api/login/qq/start
func (p *Panel) oauthQQStart(w http.ResponseWriter, r *http.Request) {
	sess, err := p.qqOAuth.Start(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "发起 QQ 扫码失败: "+err.Error())
		return
	}
	log.Printf("[panel] qq oauth start session=%s guid=%s", sess.ID[:8], sess.GUID[:8])
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"qr":         "data:image/png;base64," + base64.StdEncoding.EncodeToString(sess.QR),
		"expires_in": int((3 * time.Minute).Seconds()),
	})
}

// oauthQQPoll GET /panel/api/login/qq/poll?id=
func (p *Panel) oauthQQPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少会话 id")
		return
	}
	status, err := p.qqOAuth.Poll(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	resp := map[string]any{"status": status}
	if s := p.qqOAuth.Session(id); s != nil {
		if _, errText := s.Stat(); errText != "" {
			resp["error"] = errText
		}
	}
	if status == oauth.StatusWaiting || status == oauth.StatusScanned {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}
	if status != oauth.StatusDone {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	cred := p.qqOAuth.Credential(id)
	if cred == nil {
		resp["status"] = "error"
		resp["error"] = "会话凭证丢失，请重新扫码"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	revived, err := p.applyCredential(cred)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp["revived"] = revived
	resp["account"] = p.cfg.State.Status()
	log.Printf("[panel] qq oauth login ok openid=%s…", cred.OpenID[:min(8, len(cred.OpenID))])
	writeJSON(w, http.StatusOK, resp)
}

// oauthErrText 取会话的错误文本（无错误返回空串）。
func oauthErrText(f *oauth.Flow, id string) string {
	// Flow 未暴露会话句柄；这里经 Credential/Stat 的包装访问。
	if s := f.Session(id); s != nil {
		_, errText := s.Stat()
		return errText
	}
	return ""
}
