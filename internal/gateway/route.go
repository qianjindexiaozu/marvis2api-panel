package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
)

const (
	stickyTTL = 30 * time.Minute
	quotaTTL  = 5 * time.Minute
)

var errCooling = errors.New("账号暂不可用")

// router 把新对话分到剩余额度最多的账号，并把同一段对话固定在那个账号上。
type router struct {
	mu    sync.Mutex
	bind  map[string]routeBind
	quota map[string]routeQuota
	skip  map[string]time.Time
}

type routeBind struct {
	id string
	at time.Time
}

type routeQuota struct {
	remaining int64
	known     bool
	at        time.Time
}

func newRouter() *router {
	return &router{
		bind:  map[string]routeBind{},
		quota: map[string]routeQuota{},
		skip:  map[string]time.Time{},
	}
}

func (h *Handler) router() *router {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.routes == nil {
		h.routes = newRouter()
	}
	return h.routes
}

// chooseAccount 显式请求头优先。否则按会话粘性，再按剩余额度选号。
func (h *Handler) chooseAccount(ctx context.Context, hint string, body []byte, hdr http.Header) (*account.Auth, error) {
	key := sessionKey(hdr, body)
	if hint != "" {
		a, err := h.pickAccount(hint)
		if err == nil && a != nil && a.Dormant() {
			return nil, fmt.Errorf("这个账号已休眠")
		}
		if err == nil && a != nil && key != "" {
			h.router().bindTo(key, a.ID())
		}
		return a, err
	}
	accounts := h.listAccounts()
	if len(accounts) == 0 {
		a, err := h.pickAccount("")
		if a != nil && a.Dormant() {
			return nil, fmt.Errorf("没有可用账号")
		}
		return a, err
	}
	if len(accounts) == 1 {
		if h.cfg.State != nil && !h.cfg.State.Healthy() {
			return nil, errCooling
		}
		if key != "" && accounts[0].ID() != "" {
			h.router().bindTo(key, accounts[0].ID())
		}
		return accounts[0], nil
	}
	rt := h.router()
	h.seedQuota(rt)
	if id := rt.current(key, accounts, h.stateBlocked); id != "" {
		return h.pickAccount(id)
	}
	h.refreshQuotas(ctx, accounts, rt)
	id := rt.assign(key, accounts, h.stateBlocked)
	if id == "" {
		rt.clearSkips()
		id = rt.assign(key, accounts, h.stateBlocked)
	}
	if id == "" {
		if h.cfg.State != nil && !h.cfg.State.Healthy() {
			return nil, errCooling
		}
		return nil, fmt.Errorf("没有可用账号")
	}
	return h.pickAccount(id)
}

func (h *Handler) listAccounts() []*account.Auth {
	if h.cfg.Accounts == nil {
		if a := h.cfg.State.Auth(); a != nil && a.TokenValue() != "" && !a.Dormant() {
			return []*account.Auth{a}
		}
		return nil
	}
	list, err := h.cfg.Accounts.List()
	if err != nil {
		return nil
	}
	var out []*account.Auth
	for _, a := range list {
		if a != nil && a.TokenValue() != "" && a.ID() != "" && !a.Dormant() {
			out = append(out, a)
		}
	}
	return out
}

func (h *Handler) seedQuota(rt *router) {
	if h.cfg.State == nil {
		return
	}
	st := h.cfg.State.Status()
	a := h.cfg.State.Auth()
	if a == nil || a.ID() == "" {
		return
	}
	if st.QuotaTotal > 0 || st.QuotaRemaining > 0 {
		rt.seed(a.ID(), st.QuotaRemaining)
	}
}

func (h *Handler) refreshQuotas(ctx context.Context, accounts []*account.Auth, rt *router) {
	if h.cfg.Upstream == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	now := time.Now()
	for _, a := range accounts {
		if rt.fresh(a.ID(), now) {
			continue
		}
		wg.Add(1)
		go func(a *account.Auth) {
			defer wg.Done()
			q, err := h.cfg.Upstream.Quota(ctx, a)
			if err != nil {
				return
			}
			rt.setQuota(a.ID(), q.Remaining)
		}(a)
	}
	wg.Wait()
}

func (h *Handler) stateBlocked(id string) bool {
	if h.cfg.State == nil || h.cfg.State.Healthy() {
		return false
	}
	a := h.cfg.State.Auth()
	return a != nil && a.ID() == id
}

func (h *Handler) noteSpend(a *account.Auth, n int64) {
	if a == nil || n <= 0 {
		return
	}
	h.router().addUsage(a.ID(), n)
}

func (h *Handler) noteRouteFailure(a *account.Auth, kind upstream.ErrKind) {
	if a == nil || a.ID() == "" {
		return
	}
	switch kind {
	case upstream.ErrAuthDead, upstream.ErrHardQuota:
		h.router().fail(a.ID(), 30*time.Minute)
	case upstream.ErrSoftRate, upstream.ErrServer:
		h.router().fail(a.ID(), 2*time.Minute)
	}
}

func (h *Handler) isActive(a *account.Auth) bool {
	if h.cfg.State == nil || h.cfg.State.Auth() == nil || a == nil {
		return true
	}
	return h.cfg.State.Auth().ID() == a.ID()
}

func (r *router) bindTo(key, id string) {
	if key == "" || id == "" {
		return
	}
	r.mu.Lock()
	r.bind[key] = routeBind{id: id, at: time.Now()}
	r.mu.Unlock()
}

func (r *router) current(key string, accounts []*account.Auth, blocked func(string) bool) string {
	if key == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bind[key]
	if !ok || time.Since(b.at) > stickyTTL {
		delete(r.bind, key)
		return ""
	}
	if r.skipping(b.id) || (blocked != nil && blocked(b.id)) {
		return ""
	}
	for _, a := range accounts {
		if a.ID() == b.id {
			r.bind[key] = routeBind{id: b.id, at: time.Now()}
			return b.id
		}
	}
	delete(r.bind, key)
	return ""
}

func (r *router) assign(key string, accounts []*account.Auth, blocked func(string) bool) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *account.Auth
	var bestRem int64
	bestPositive := false
	bestKnown := false
	for _, a := range accounts {
		id := a.ID()
		if id == "" || r.skipping(id) || (blocked != nil && blocked(id)) {
			continue
		}
		q, ok := r.quota[id]
		positive := ok && q.known && q.remaining > 0
		if best == nil || betterQuota(positive, q.remaining, ok && q.known, bestPositive, bestRem, bestKnown, id, best.ID()) {
			best = a
			bestRem = q.remaining
			bestPositive = positive
			bestKnown = ok && q.known
		}
	}
	if best == nil {
		return ""
	}
	if key != "" {
		r.bind[key] = routeBind{id: best.ID(), at: time.Now()}
	}
	return best.ID()
}

func betterQuota(pos bool, rem int64, known, bestPos bool, bestRem int64, bestKnown bool, id, bestID string) bool {
	if pos != bestPos {
		return pos
	}
	if pos && rem != bestRem {
		return rem > bestRem
	}
	if !pos && known != bestKnown {
		return !known
	}
	return id < bestID
}

func (r *router) seed(id string, remaining int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.quota[id]; ok {
		return
	}
	r.quota[id] = routeQuota{remaining: remaining, known: true, at: time.Now()}
}

func (r *router) setQuota(id string, remaining int64) {
	r.mu.Lock()
	r.quota[id] = routeQuota{remaining: remaining, known: true, at: time.Now()}
	r.mu.Unlock()
}

func (r *router) fresh(id string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	q, ok := r.quota[id]
	return ok && q.known && now.Sub(q.at) < quotaTTL
}

func (r *router) addUsage(id string, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q, ok := r.quota[id]
	if !ok || !q.known {
		return
	}
	q.remaining -= n
	if q.remaining < 0 {
		q.remaining = 0
	}
	r.quota[id] = q
}

func (r *router) fail(id string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skip[id] = time.Now().Add(d)
	if q, ok := r.quota[id]; ok {
		q.remaining = 0
		r.quota[id] = q
	}
	for key, b := range r.bind {
		if b.id == id {
			delete(r.bind, key)
		}
	}
}

func (r *router) skipping(id string) bool {
	until, ok := r.skip[id]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(r.skip, id)
	return false
}

func (r *router) clearSkips() {
	r.mu.Lock()
	r.skip = map[string]time.Time{}
	r.mu.Unlock()
}

func sessionKey(hdr http.Header, body []byte) string {
	if hdr != nil {
		for _, name := range []string{"X-Marvis-Session", "X-Conversation-Id"} {
			if v := strings.TrimSpace(hdr.Get(name)); v != "" {
				return "h:" + v
			}
		}
	}
	if v := conversationID(body); v != "" {
		return "c:" + v
	}
	if v := firstUserText(body); v != "" {
		sum := sha256.Sum256([]byte(v))
		return "m:" + hex.EncodeToString(sum[:16])
	}
	return ""
}

func conversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		for _, k := range []string{"conversation_id", "conversationId"} {
			if v := jsonString(meta[k]); v != "" {
				return v
			}
		}
	}
	for _, k := range []string{"conversation_id", "conversationId"} {
		if v := jsonString(obj[k]); v != "" {
			return v
		}
	}
	return ""
}

func firstUserText(body []byte) string {
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	text := ""
	for _, m := range obj.Messages {
		part := messageText(m.Content)
		if part == "" {
			continue
		}
		if m.Role == "user" || m.Role == "" {
			text = part
			break
		}
		if text == "" {
			text = part
		}
	}
	if len(text) > 2000 {
		text = text[:2000]
	}
	return text
}

func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return strings.TrimSpace(b.String())
}

func jsonString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
