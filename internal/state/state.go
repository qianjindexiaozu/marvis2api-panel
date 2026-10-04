// Package state 单账号的运行时状态：冷却 / 熔断 / 在途租约 / 统计 / 配额观测，
// 以及 state.json 的落盘与恢复。
//
// Marvis 客户端同时只能登录一个账号，网关不设账号池——没有选号、没有换号重试。
// 冷却与熔断保留的意义：上游限流 / 认证失效时，让网关在冷却期内直接拒绝请求
// （而不是继续打上游把账号往死里推），并把恢复时刻透出给面板与客户端。
package state

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
)

// CoolKind 冷却类别：Soft = 限流等可自动恢复的短冷却；Hard = 配额耗尽/认证失败等
// 长冷却（换 token 或面板复活后恢复）。
type CoolKind string

const (
	CoolSoft CoolKind = "soft"
	CoolHard CoolKind = "hard"
)

// ErrCategory 上游错误类别（gateway 侧分类后传入）。
type ErrCategory int

const (
	ErrAuth     ErrCategory = iota // 401/403：token 无效 → 硬冷却（人工换 token）
	ErrQuota                       // 402/配额耗尽 → 硬冷却至配额重置（次日）
	ErrRate                        // 429：限流 → 软冷却（有界退避）
	ErrNotFound                    // 404：上游偶发 → 固定短冷却（不喂熔断）
	ErrServer                      // 5xx / 网络：瞬时故障 → 熔断失败累计
)

// State 单账号运行时状态。并发安全。
type State struct {
	mu sync.RWMutex
	a  *account.Auth // nil = 未配置凭证

	inFlight atomic.Int64

	// 冷却状态（until 零值 = 未冷却）。coolKind 区分软/硬；softStreak 记录
	// 连续软冷却次数（有界指数退避用）；reason 人读原因（面板透出）。
	until      time.Time
	coolKind   CoolKind
	softStreak int
	reason     string

	// 熔断器：fails 连续失败计数（成功清零）；retryCount 熔断轮次（放大退避）；
	// breakerUntil 熔断截止时刻（零值 = 未熔断）。
	fails        int
	retryCount   int
	breakerUntil time.Time

	// 观测统计：successCount / errTotal 终身累计；lastError/lastErrorAt 最近失败详情。
	successCount int64
	errTotal     int64
	lastError    string
	lastErrorAt  time.Time

	// 配额观测（scheduler 周期刷新）。
	quotaRemaining int64
	quotaTotal     int64
	quotaUpdatedAt time.Time

	// 探测结果（scheduler / 面板探活写入，观测用）。
	lastProbeAt   time.Time
	lastProbeOK   bool
	lastLatencyMs int64

	// 可调参数（main 注入）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	softRateMax        time.Duration
	notFoundCooldown   time.Duration
	authDeadCooldown   time.Duration
	maxInFlight        int

	stateFp   string
	dirty     atomic.Bool
	persistN  int
	stopCh    chan struct{}
	closeOnce sync.Once
}

// 默认参数。
const (
	defaultBreakerThreshold   = 3
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
	defaultSoftRateMax        = 2 * time.Hour
	defaultNotFoundCooldown   = 60 * time.Second
	defaultAuthDeadCooldown   = 24 * time.Hour
)

// New 构建状态机；stateFp 非空时恢复本地状态并启动周期落盘。
func New(stateFp string) *State {
	s := &State{
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		softRateMax:        defaultSoftRateMax,
		notFoundCooldown:   defaultNotFoundCooldown,
		authDeadCooldown:   defaultAuthDeadCooldown,
	}
	if stateFp != "" {
		s.load()
		s.startFlusher()
	}
	return s
}

// Close 停止后台落盘并做最后一次落盘（幂等）。
func (s *State) Close() {
	if s.stopCh == nil {
		return
	}
	s.closeOnce.Do(func() { close(s.stopCh) })
	s.Flush()
}

// ---------------------------------------------------------------------------
// 参数注入
// ---------------------------------------------------------------------------

// SetBreaker 注入熔断器参数。非正值保留原值。
func (s *State) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threshold > 0 {
		s.breakerThreshold = threshold
	}
	if cooldown > 0 {
		s.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		s.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避封顶。非正值保留原值。
func (s *State) SetSoftRateMax(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > 0 {
		s.softRateMax = d
	}
}

// SetNotFoundCooldown 注入 404 固定短冷却时长。非正值保留原值。
func (s *State) SetNotFoundCooldown(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > 0 {
		s.notFoundCooldown = d
	}
}

// SetAuthDeadCooldown 注入认证失败硬冷却时长。非正值保留原值。
func (s *State) SetAuthDeadCooldown(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > 0 {
		s.authDeadCooldown = d
	}
}

// SetMaxInFlight 注入最大并发在途请求数；0 = 不限。负值保留原值。
func (s *State) SetMaxInFlight(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n >= 0 {
		s.maxInFlight = n
	}
}

// ---------------------------------------------------------------------------
// 凭证
// ---------------------------------------------------------------------------

// SetAuth 载入/替换凭证（面板保存、目录热加载、refresh 写回共用）。
// 换过凭证时自动解除硬冷却：换完还得再点一次复活太反直觉。
func (s *State) SetAuth(a *account.Auth) (credChanged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.a != nil && a != nil {
		credChanged = a.TokenValue() != s.a.TokenValue() || a.RefreshTokenValue() != s.a.RefreshTokenValue()
	}
	s.a = a
	if credChanged && s.coolKind == CoolHard {
		s.reviveLocked()
	}
	s.dirty.Store(true)
	return credChanged
}

// Auth 返回凭证副本（无锁共享，调用方可安全读写）；未配置返回 nil。
func (s *State) Auth() *account.Auth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.a == nil {
		return nil
	}
	return s.a.Clone()
}

// HasAuth 报告是否已配置凭证。
func (s *State) HasAuth() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.a != nil && s.a.TokenValue() != ""
}

// ---------------------------------------------------------------------------
// 健康与在途租约
// ---------------------------------------------------------------------------

// Healthy 报告账号当前是否可用：已配置凭证、未冷却、未熔断。
func (s *State) Healthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.healthyLocked(time.Now())
}

func (s *State) healthyLocked(now time.Time) bool {
	if s.a == nil || s.a.TokenValue() == "" {
		return false
	}
	if !s.until.IsZero() && now.Before(s.until) {
		return false
	}
	if !s.breakerUntil.IsZero() && now.Before(s.breakerUntil) {
		return false
	}
	return true
}

// Acquire 占一个在途名额；上限内返回 true。上限 0 = 不限（仍计数观测）。
func (s *State) Acquire() bool {
	s.mu.RLock()
	limit := s.maxInFlight
	s.mu.RUnlock()
	if limit <= 0 {
		s.inFlight.Add(1)
		return true
	}
	for {
		cur := s.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if s.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额（幂等，减到 0 为止）。
func (s *State) Release() {
	for {
		cur := s.inFlight.Load()
		if cur <= 0 {
			return
		}
		if s.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// inFlightFull 报告在途是否占满（limit=0 不限 → 恒 false）。调用方需已持读锁。
func (s *State) inFlightFull() bool {
	if s.maxInFlight <= 0 {
		return false
	}
	return s.inFlight.Load() >= int64(s.maxInFlight)
}

// ---------------------------------------------------------------------------
// 冷却与熔断
// ---------------------------------------------------------------------------

// NoteError 记一次失败并按类别处置：
//
//   - ErrAuth：硬冷却 authDeadCooldown（换 token 后面板保存自动复活）；
//   - ErrQuota：硬冷却至配额重置（上游明示 resetAt 优先，否则次日 04:00）；
//   - ErrRate：软冷却（base 来自配置），有界指数退避封顶 softRateMax；
//   - ErrNotFound：固定短冷却（不喂熔断）；
//   - ErrServer：熔断失败累计，达阈值按指数退避熔断。
func (s *State) NoteError(cat ErrCategory, base time.Duration, resetAt time.Time, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.errTotal++
	s.lastError = reason
	s.lastErrorAt = now
	switch cat {
	case ErrAuth:
		s.until = now.Add(s.authDeadCooldownOr())
		s.coolKind = CoolHard
	case ErrQuota:
		s.until = s.quotaResetLocked(now, resetAt)
		s.coolKind = CoolHard
	case ErrRate:
		if !resetAt.IsZero() {
			s.until = s.cappedSoftUntilLocked(now, resetAt)
		} else if s.coolKind != CoolSoft || !now.Before(s.until) {
			s.softStreak++
			s.until = now.Add(s.softDurationLocked(base, s.softStreak))
		}
		s.coolKind = CoolSoft
	case ErrNotFound:
		s.until = now.Add(s.notFoundCooldownOr())
		s.coolKind = CoolSoft
	default:
		s.recordBreakerFailureLocked()
	}
	s.reason = reason
	s.dirty.Store(true)
}

// NoteSuccess 记一次成功：清失败计数与软冷却（实测已恢复）；熔断同样清除。
// 硬冷却不清——认证/配额问题不会因为一次成功而消失。
func (s *State) NoteSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.successCount++
	s.fails = 0
	s.softStreak = 0
	if s.coolKind == CoolSoft {
		s.until = time.Time{}
		s.coolKind = ""
		s.reason = ""
	}
	s.breakerUntil = time.Time{}
	s.dirty.Store(true)
}

// Revive 手动复活：清冷却 + 熔断 + 失败计数（面板「解除冷却」按钮）。
func (s *State) Revive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reviveLocked()
	s.dirty.Store(true)
}

func (s *State) reviveLocked() {
	s.until = time.Time{}
	s.coolKind = ""
	s.softStreak = 0
	s.reason = ""
	s.breakerUntil = time.Time{}
	s.fails = 0
	s.retryCount = 0
}

// SetQuota 记录配额观测（scheduler 周期刷新 / 面板手工查询）。
func (s *State) SetQuota(remaining, total int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaRemaining = remaining
	s.quotaTotal = total
	s.quotaUpdatedAt = time.Now()
	s.dirty.Store(true)
}

// UnfreezeQuota 配额恢复解冻：仅当处于配额语义的硬冷却且新配额 > 0 时解除。
// 每日配额重置后账号自动回池，无需人工。
func (s *State) UnfreezeQuota(remaining int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaRemaining = remaining
	s.quotaUpdatedAt = time.Now()
	if remaining > 0 && s.coolKind == CoolHard && !s.until.IsZero() && time.Now().Before(s.until) &&
		s.reason == "quota exhausted" {
		s.until = time.Time{}
		s.coolKind = ""
		s.reason = ""
	}
	s.dirty.Store(true)
}

// SetProbeResult 记录探测结果（观测用）。
func (s *State) SetProbeResult(ok bool, latency time.Duration, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastProbeAt = time.Now()
	s.lastProbeOK = ok
	s.lastLatencyMs = latency.Milliseconds()
	if errText != "" {
		s.lastError = errText
		s.lastErrorAt = s.lastProbeAt
	}
}

// ---------------------------------------------------------------------------
// 内部计算
// ---------------------------------------------------------------------------

// cappedSoftUntilLocked 把上游重置墙钟截断到 softRateMax。
func (s *State) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(s.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

func (s *State) softRateMaxOr() time.Duration {
	if s.softRateMax > 0 {
		return s.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数指数放大，封顶 softRateMax。
func (s *State) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > 20 {
		shift = 20
	}
	d <<= shift
	max := s.softRateMaxOr()
	if d > max || d <= 0 {
		d = max
	}
	return d
}

// quotaResetLocked 配额硬冷却截止：上游明示 resetAt 优先，否则次日 04:00。
func (s *State) quotaResetLocked(now, resetAt time.Time) time.Time {
	if !resetAt.IsZero() && resetAt.After(now) {
		return resetAt
	}
	t := now.AddDate(0, 0, 1)
	return time.Date(t.Year(), t.Month(), t.Day(), 4, 0, 0, 0, t.Location())
}

// recordBreakerFailureLocked 累计一次熔断失败；达阈值按指数退避熔断。
func (s *State) recordBreakerFailureLocked() {
	s.fails++
	if s.fails < s.breakerThreshold {
		return
	}
	d := s.breakerCooldown
	for i := 0; i < s.retryCount; i++ {
		d *= 2
		if d >= s.breakerCooldownMax {
			d = s.breakerCooldownMax
			break
		}
	}
	s.fails = 0
	s.retryCount++
	s.breakerUntil = time.Now().Add(d)
}

func (s *State) authDeadCooldownOr() time.Duration {
	if s.authDeadCooldown > 0 {
		return s.authDeadCooldown
	}
	return defaultAuthDeadCooldown
}

func (s *State) notFoundCooldownOr() time.Duration {
	if s.notFoundCooldown > 0 {
		return s.notFoundCooldown
	}
	return defaultNotFoundCooldown
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

// persisted 落盘形态（凭证在 auth.json，不在这里）。
type persisted struct {
	CoolKind       CoolKind  `json:"cool_kind,omitempty"`
	Until          time.Time `json:"until,omitempty"`
	SoftStreak     int       `json:"soft_streak,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Fails          int       `json:"fails,omitempty"`
	RetryCount     int       `json:"retry_count,omitempty"`
	BreakerUntil   time.Time `json:"breaker_until,omitempty"`
	SuccessCount   int64     `json:"success_count,omitempty"`
	ErrTotal       int64     `json:"err_total,omitempty"`
	QuotaRemaining int64     `json:"quota_remaining,omitempty"`
	QuotaTotal     int64     `json:"quota_total,omitempty"`
}

type stateFile struct {
	Version int       `json:"version"`
	SavedAt time.Time `json:"saved_at"`
	Account persisted `json:"account"`
}

const stateVersion = 2

// load 启动时恢复状态；文件缺失/损坏静默降级（按全新状态启动）。
func (s *State) load() {
	raw, err := os.ReadFile(s.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		log.Printf("WARN: [state] %s 解析失败，按全新状态启动: %v", s.stateFp, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := sf.Account
	s.coolKind = p.CoolKind
	s.until = p.Until
	s.softStreak = p.SoftStreak
	s.reason = p.Reason
	s.fails = p.Fails
	s.retryCount = p.RetryCount
	s.breakerUntil = p.BreakerUntil
	s.successCount = p.SuccessCount
	s.errTotal = p.ErrTotal
	s.quotaRemaining = p.QuotaRemaining
	s.quotaTotal = p.QuotaTotal
}

// saveLocked 原子落盘。调用方必须已持有 s.mu。
func (s *State) saveLocked() {
	if s.stateFp == "" {
		return
	}
	sf := stateFile{
		Version: stateVersion,
		SavedAt: time.Now(),
		Account: persisted{
			CoolKind:       s.coolKind,
			Until:          s.until,
			SoftStreak:     s.softStreak,
			Reason:         s.reason,
			Fails:          s.fails,
			RetryCount:     s.retryCount,
			BreakerUntil:   s.breakerUntil,
			SuccessCount:   s.successCount,
			ErrTotal:       s.errTotal,
			QuotaRemaining: s.quotaRemaining,
			QuotaTotal:     s.quotaTotal,
		},
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.stateFp), 0o755); err != nil {
		s.persistFail(err)
		return
	}
	tmp := s.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		s.persistFail(err)
		return
	}
	if err := os.Rename(tmp, s.stateFp); err != nil {
		_ = os.Remove(tmp)
		s.persistFail(err)
		return
	}
	if s.persistN > 0 {
		log.Printf("[state] %s 落盘恢复（此前连续失败 %d 次）", s.stateFp, s.persistN)
		s.persistN = 0
	}
	s.dirty.Store(false)
}

// persistFail 落盘失败计数 + 日志节流。调用方必须已持有 s.mu。
func (s *State) persistFail(err error) {
	s.persistN++
	if s.persistN == 1 || s.persistN%20 == 0 {
		log.Printf("WARN: [state] %s 落盘失败（连续 %d 次）: %v", s.stateFp, s.persistN, err)
	}
}

// startFlusher 周期落盘（30s 防抖：只在 dirty 时写）。
func (s *State) startFlusher() {
	s.stopCh = make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-t.C:
				s.Flush()
			}
		}
	}()
}

// Flush dirty 时立即落盘（面板操作 / 停机前调用）。
func (s *State) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty.Load() {
		s.saveLocked()
	}
}
