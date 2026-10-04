package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/kernel"
)

type stateStatus struct {
	state     string
	remaining int64
	total     int64
	reason    string
	lastError string
	probeOK   bool
	latency   int64
	success   int64
	errs      int64
}

// adopt 把账号写入目录，设为当前账号，并启动它自己的内核。
func (p *Panel) adopt(a *account.Auth) (bool, error) {
	if p.cfg.Accounts != nil {
		if err := p.cfg.Accounts.Save(a); err != nil {
			return false, err
		}
		if err := p.cfg.Accounts.SetActive(a.ID()); err != nil {
			return false, err
		}
	}
	if err := p.cfg.File.Save(a); err != nil {
		return false, err
	}
	changed := p.cfg.State.SetAuth(a)
	p.cfg.State.Flush()
	if p.cfg.Kernels != nil {
		p.cfg.Kernels.SetActive(a.ID())
		p.cfg.Kernels.Ensure(a)
	}
	return changed, nil
}

func (p *Panel) accountsList(w http.ResponseWriter, r *http.Request) {
	var items []map[string]any
	active := ""
	if p.cfg.Accounts != nil {
		active = p.cfg.Accounts.Active()
		list, err := p.cfg.Accounts.List()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		var st stateStatus
		if p.cfg.State != nil {
			s := p.cfg.State.Status()
			st = stateStatus{s.State, s.QuotaRemaining, s.QuotaTotal, s.Reason, s.LastError, s.LastProbeOK, s.LatencyMs, s.SuccessCount, s.ErrTotal}
		}
		p.refreshAccountFacts(r.Context(), list)
		for _, a := range list {
			view := kernel.View{ID: a.ID(), Kernel: "stopped"}
			if p.cfg.Kernels != nil {
				view = p.cfg.Kernels.View(a.ID())
			}
			remain, total := p.cachedQuota(a.ID(), a.ID() == active, st)
			item := map[string]any{
				"id": a.ID(), "name": displayName(a), "name_error": p.nameError(a.ID()), "uid": a.UID, "login_type": a.LoginType,
				"dormant": a.Dormant(), "kernel": view.Kernel, "error": view.Error,
				"expires_at": a.ExpiresAtValue(), "has_refresh": a.RefreshTokenValue() != "",
				"note": a.Note, "created_at": a.CreatedAt,
				"quota_remaining": remain, "quota_total": total,
			}
			if a.ID() == active {
				item["state"] = st.state
				item["reason"] = st.reason
				item["last_error"] = st.lastError
				item["last_probe_ok"] = st.probeOK
				item["latency_ms"] = st.latency
				item["success_count"] = st.success
				item["err_total"] = st.errs
			}
			items = append(items, item)
		}
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": items, "active": active,
		"kernel_installed": kernel.Installed(),
	})
}

func (p *Panel) accountSelect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, http.StatusBadRequest, "缺少账号 id")
		return
	}
	if p.cfg.Accounts == nil {
		writeErr(w, http.StatusNotFound, "未启用多账号")
		return
	}
	a, err := p.cfg.Accounts.Load(req.ID)
	if err != nil || a == nil {
		writeErr(w, http.StatusNotFound, "没有这个账号")
		return
	}
	if _, err := p.adopt(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": a.ID()})
}

func (p *Panel) accountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少账号 id")
		return
	}
	if err := p.removeAccount(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) removeAccount(id string) error {
	if p.cfg.Kernels != nil && id != "" {
		p.cfg.Kernels.Remove(id)
	}
	if p.cfg.Accounts != nil && id != "" {
		if err := p.cfg.Accounts.Delete(id); err != nil {
			return err
		}
	}
	active := ""
	if p.cfg.Accounts != nil {
		active = p.cfg.Accounts.Active()
	}
	if id != "" && active != "" && active != id {
		return nil
	}
	var next *account.Auth
	if p.cfg.Accounts != nil {
		list, _ := p.cfg.Accounts.List()
		if len(list) > 0 {
			next = list[0]
		}
	}
	if next == nil {
		_ = p.cfg.File.Delete()
		p.cfg.State.SetAuth(nil)
		if p.cfg.Accounts != nil {
			_ = p.cfg.Accounts.SetActive("")
		}
		if p.cfg.Kernels != nil {
			p.cfg.Kernels.SetActive("")
		}
		return nil
	}
	_, err := p.adopt(next)
	return err
}

func (p *Panel) accountByID(id string) (*account.Auth, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		if p.cfg.State == nil || p.cfg.State.Auth() == nil {
			return nil, fmt.Errorf("尚未配置凭证")
		}
		return p.cfg.State.Auth(), nil
	}
	if p.cfg.Accounts == nil {
		return nil, fmt.Errorf("未启用多账号")
	}
	a, err := p.cfg.Accounts.Load(id)
	if err != nil || a == nil || a.TokenValue() == "" {
		return nil, fmt.Errorf("没有这个账号")
	}
	return a, nil
}

func (p *Panel) accountProbe(w http.ResponseWriter, r *http.Request) {
	a, err := p.accountByID(r.URL.Query().Get("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.ProbeTimeout)
	defer cancel()
	started := time.Now()
	_, perr := p.cfg.Upstream.ModelsLive(ctx, a)
	d := time.Since(started)
	if p.cfg.State != nil && p.cfg.State.Auth() != nil && p.cfg.State.Auth().ID() == a.ID() {
		if perr != nil {
			p.cfg.State.SetProbeResult(false, d, "probe: "+publicErr(perr))
		} else {
			p.cfg.State.SetProbeResult(true, d, "")
		}
	}
	nick := p.refreshName(ctx, a)
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": publicErr(perr), "latency_ms": d.Milliseconds(), "name": nick})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": d.Milliseconds(), "name": nick})
}

func (p *Panel) accountQuota(w http.ResponseWriter, r *http.Request) {
	a, err := p.accountByID(r.URL.Query().Get("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream client not available")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.cfg.ProbeTimeout)
	defer cancel()
	q, err := p.cfg.Upstream.Quota(ctx, a)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	p.storeQuota(a.ID(), q.Remaining, q.Total)
	if p.cfg.State != nil && p.cfg.State.Auth() != nil && p.cfg.State.Auth().ID() == a.ID() {
		p.cfg.State.SetQuota(q.Remaining, q.Total)
		p.cfg.State.UnfreezeQuota(q.Remaining)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "remaining": q.Remaining, "total": q.Total})
}

type quotaSnap struct {
	remaining int64
	total     int64
	at        time.Time
}

func displayName(a *account.Auth) string {
	if a == nil {
		return ""
	}
	name := strings.TrimSpace(a.Name)
	if name == "" || name == a.UID || name == a.ID() {
		return ""
	}
	return name
}

func (p *Panel) refreshAccountFacts(ctx context.Context, list []*account.Auth) {
	if p == nil || p.cfg.Upstream == nil || len(list) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, a := range list {
		wg.Add(1)
		go func(a *account.Auth) {
			defer wg.Done()
			p.fillName(ctx, a)
			p.fillQuota(ctx, a)
		}(a)
	}
	wg.Wait()
}

func (p *Panel) noteNameErr(id, msg string) {
	p.nameMu.Lock()
	defer p.nameMu.Unlock()
	if p.nameErr == nil {
		p.nameErr = map[string]string{}
	}
	if msg == "" {
		delete(p.nameErr, id)
		return
	}
	p.nameErr[id] = msg
}

func (p *Panel) nameError(id string) string {
	p.nameMu.Lock()
	defer p.nameMu.Unlock()
	return p.nameErr[id]
}

// refreshName 向微信或 QQ 拉展示名并写回。探活时强制刷新，失败不影响探活结果。
func (p *Panel) refreshName(ctx context.Context, a *account.Auth) string {
	if p == nil || p.cfg.Upstream == nil || a == nil || strings.TrimSpace(a.UID) == "" {
		return displayName(a)
	}
	nick, err := p.cfg.Upstream.UserInfo(ctx, a)
	if err != nil || nick == "" || nick == a.UID {
		if err != nil {
			p.noteNameErr(a.ID(), err.Error())
		}
		return displayName(a)
	}
	p.noteNameErr(a.ID(), "")
	if nick == strings.TrimSpace(a.Name) {
		return nick
	}
	a.SetName(nick)
	if p.cfg.Accounts != nil {
		_ = p.cfg.Accounts.Save(a)
	}
	if p.cfg.State != nil && p.cfg.State.Auth() != nil && p.cfg.State.Auth().ID() == a.ID() {
		cur := p.cfg.State.Auth()
		if cur.TokenValue() == a.TokenValue() {
			cur.SetName(nick)
			p.cfg.State.SetAuth(cur)
		}
	}
	return nick
}

func (p *Panel) fillName(ctx context.Context, a *account.Auth) {
	if a == nil || displayName(a) != "" || strings.TrimSpace(a.UID) == "" {
		return
	}
	p.nameMu.Lock()
	if p.nameTry == nil {
		p.nameTry = map[string]time.Time{}
	}
	if t, ok := p.nameTry[a.ID()]; ok && time.Since(t) < 10*time.Minute {
		p.nameMu.Unlock()
		return
	}
	p.nameTry[a.ID()] = time.Now()
	p.nameMu.Unlock()
	nick, err := p.cfg.Upstream.UserInfo(ctx, a)
	if err != nil || nick == "" || nick == a.UID {
		return
	}
	a.SetName(nick)
	if p.cfg.Accounts != nil {
		_ = p.cfg.Accounts.Save(a)
	}
	if p.cfg.State != nil && p.cfg.State.Auth() != nil && p.cfg.State.Auth().ID() == a.ID() {
		cur := p.cfg.State.Auth()
		if cur.TokenValue() == a.TokenValue() {
			cur.SetName(nick)
			p.cfg.State.SetAuth(cur)
		}
	}
}

func (p *Panel) fillQuota(ctx context.Context, a *account.Auth) {
	if a == nil {
		return
	}
	p.quotaMu.Lock()
	if p.quotas == nil {
		p.quotas = map[string]quotaSnap{}
	}
	if q, ok := p.quotas[a.ID()]; ok && time.Since(q.at) < 2*time.Minute {
		p.quotaMu.Unlock()
		return
	}
	p.quotaMu.Unlock()
	q, err := p.cfg.Upstream.Quota(ctx, a)
	if err != nil {
		return
	}
	p.storeQuota(a.ID(), q.Remaining, q.Total)
}

func (p *Panel) storeQuota(id string, remaining, total int64) {
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	if p.quotas == nil {
		p.quotas = map[string]quotaSnap{}
	}
	p.quotas[id] = quotaSnap{remaining: remaining, total: total, at: time.Now()}
}

func (p *Panel) cachedQuota(id string, active bool, st stateStatus) (int64, int64) {
	p.quotaMu.Lock()
	q, ok := p.quotas[id]
	p.quotaMu.Unlock()
	if ok && (q.total > 0 || q.remaining > 0) {
		return q.remaining, q.total
	}
	if active && (st.total > 0 || st.remaining > 0) {
		return st.remaining, st.total
	}
	return 0, 0
}

func (p *Panel) accountDormant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Dormant bool   `json:"dormant"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.ID == "" {
		writeErr(w, http.StatusBadRequest, "缺少账号 id")
		return
	}
	a, err := p.accountByID(req.ID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	a.SetDormant(req.Dormant)
	if p.cfg.Accounts == nil {
		writeErr(w, http.StatusNotFound, "未启用多账号")
		return
	}
	if err := p.cfg.Accounts.Save(a); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "dormant": req.Dormant})
}

func (p *Panel) accountRefreshToken(w http.ResponseWriter, r *http.Request) {
	a, err := p.accountByID(r.URL.Query().Get("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if p.cfg.Upstream == nil || p.cfg.Upstream.RefreshPath == "" {
		writeErr(w, http.StatusNotImplemented, "未配置 upstream.refresh_path，无法刷新 token")
		return
	}
	if strings.TrimSpace(a.RefreshTokenValue()) == "" {
		writeErr(w, http.StatusBadRequest, "这个账号没有 refresh_token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := p.cfg.Upstream.RefreshToken(ctx, a); err != nil {
		log.Printf("[panel] account refresh-token err=%v", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": publicErr(err)})
		return
	}
	if p.cfg.Accounts != nil {
		if err := p.cfg.Accounts.Save(a); err != nil {
			writeErr(w, http.StatusInternalServerError, "新 token 落盘失败: "+err.Error())
			return
		}
	}
	if p.cfg.State != nil && p.cfg.State.Auth() != nil && p.cfg.State.Auth().ID() == a.ID() {
		if err := p.cfg.File.Save(a); err != nil {
			writeErr(w, http.StatusInternalServerError, "新 token 落盘失败: "+err.Error())
			return
		}
		p.cfg.State.SetAuth(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
