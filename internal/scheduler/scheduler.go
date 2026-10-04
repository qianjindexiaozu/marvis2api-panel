// Package scheduler 后台任务：配额刷新（分钟级）/ token 保活（每日整点）/
// 健康探测（可选周期）。三类任务各有独立开关，互不影响。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/qianjindexiaozu/marvis2api-panel/internal/account"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/state"
	"github.com/qianjindexiaozu/marvis2api-panel/internal/upstream"
)

// refreshAhead 距过期不足该时长就主动刷新。access token 有效期 7200 秒。
const refreshAhead = 10 * time.Minute

// Config 调度器配置。
type Config struct {
	State    *state.State
	Upstream *upstream.Client
	File     *account.File  // 保活刷新当前账号后写 auth.json
	Accounts *account.Store // 多账号目录。临近过期时每个号都刷新。

	// QuotaRefreshMinutes 配额刷新周期（分钟）；0 = 关闭。
	QuotaRefreshMinutes int
	// KeepaliveHours 每日本地时区整点 token 保活；空 = 关闭。
	KeepaliveHours []int
	// ProbeIntervalSeconds 健康探测周期（秒）；0 = 关闭。
	ProbeIntervalSeconds int
	// Timeout 单次上游短 RPC 超时。
	Timeout time.Duration
}

// Scheduler 后台调度器。Run 阻塞直到 ctx 取消。
type Scheduler struct {
	cfg         Config
	lastRefresh time.Time
	lastTry     map[string]time.Time
}

// New 构建调度器。
func New(cfg Config) *Scheduler {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	return &Scheduler{cfg: cfg}
}

// backgroundTasksOn 报告是否至少有一类后台任务要跑（临近过期自动刷新计入）。
func (s *Scheduler) backgroundTasksOn() bool {
	c := s.cfg
	expiryRefresh := c.Upstream != nil && c.Upstream.RefreshPath != ""
	return c.QuotaRefreshMinutes > 0 || len(c.KeepaliveHours) > 0 || c.ProbeIntervalSeconds > 0 || expiryRefresh
}

// Run 阻塞运行（ctx 取消退出）。三类任务各自记上次执行时刻：
//   - 配额刷新按 ticker；
//   - 保活按「整点命中且当天未跑」判定（跨日重置）。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.backgroundTasksOn() {
		log.Printf("[scheduler] 全部后台任务关闭：调度器空转等待退出")
		<-ctx.Done()
		return
	}
	quotaEvery := time.Duration(s.cfg.QuotaRefreshMinutes) * time.Minute
	var quotaLast time.Time
	keepaliveDoneDate := "" // "2006-01-02"（本地）
	probeEvery := time.Duration(s.cfg.ProbeIntervalSeconds) * time.Second
	var probeLast time.Time

	log.Printf("[scheduler] 后台任务: quota_refresh=%v keepalive_hours=%v probe=%v",
		quotaEvery, s.cfg.KeepaliveHours, probeEvery)

	// 启动即先跑一轮配额刷新（服务刚起，面板就有数据）。
	if quotaEvery > 0 {
		s.roundQuota(ctx)
		quotaLast = time.Now()
	}

	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if quotaEvery > 0 && now.Sub(quotaLast) >= quotaEvery {
				quotaLast = now
				s.roundQuota(ctx)
			}
			if len(s.cfg.KeepaliveHours) > 0 {
				today := now.Format("2006-01-02")
				for _, h := range s.cfg.KeepaliveHours {
					if now.Hour() == h && now.Minute() == 0 && keepaliveDoneDate != today {
						keepaliveDoneDate = today
						s.roundKeepalive(ctx)
						break
					}
				}
			}
			if probeEvery > 0 && now.Sub(probeLast) >= probeEvery {
				probeLast = now
				s.roundProbe(ctx)
			}
			// 每个账号在过期前 10 分钟刷新。失败 1 分钟后再试，避免每秒打上游。
			if s.cfg.Upstream != nil && s.cfg.Upstream.RefreshPath != "" {
				s.refreshDue(ctx, now)
			}
		}
	}
}

// refreshDue 刷新所有将在 10 分钟内过期的账号。
func (s *Scheduler) refreshDue(ctx context.Context, now time.Time) {
	for _, a := range s.accounts() {
		if !needsRefresh(a, now) {
			continue
		}
		id := a.ID()
		if id == "" {
			continue
		}
		if last, ok := s.lastTry[id]; ok && now.Sub(last) < time.Minute {
			continue
		}
		if s.lastTry == nil {
			s.lastTry = map[string]time.Time{}
		}
		s.lastTry[id] = now
		s.refreshOne(ctx, a)
	}
}

func needsRefresh(a *account.Auth, now time.Time) bool {
	if a == nil || a.RefreshTokenValue() == "" {
		return false
	}
	exp := a.ExpiresAtValue()
	if exp <= 0 {
		return false
	}
	return !time.Unix(exp, 0).After(now.Add(refreshAhead))
}

func (s *Scheduler) accounts() []*account.Auth {
	if s.cfg.Accounts != nil {
		list, err := s.cfg.Accounts.List()
		if err == nil && len(list) > 0 {
			return list
		}
	}
	if a := s.current(); a != nil {
		return []*account.Auth{a}
	}
	return nil
}

func (s *Scheduler) refreshOne(ctx context.Context, a *account.Auth) {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	if err := s.cfg.Upstream.RefreshToken(cctx, a); err != nil {
		log.Printf("WARN: [scheduler] 刷新 token 失败 id=%s err=%v", a.ID(), err)
		return
	}
	if s.cfg.Accounts != nil {
		if err := s.cfg.Accounts.Save(a); err != nil {
			log.Printf("WARN: [scheduler] 刷新后写入账号目录失败: %v", err)
		}
	}
	activeID := ""
	if s.cfg.State != nil && s.cfg.State.Auth() != nil {
		activeID = s.cfg.State.Auth().ID()
	}
	if activeID == "" || activeID == a.ID() {
		if s.cfg.File != nil {
			if err := s.cfg.File.Save(a); err != nil {
				log.Printf("WARN: [scheduler] 刷新后写入 auth.json 失败: %v", err)
				return
			}
		}
		if s.cfg.State != nil {
			s.cfg.State.SetAuth(a)
		}
	}
	s.lastRefresh = time.Now()
	log.Printf("[scheduler] token 已刷新 id=%s", a.ID())
}

// current 取当前凭证；未配置返回 nil（后台任务静默跳过）。
func (s *Scheduler) current() *account.Auth {
	if s.cfg.State == nil {
		return nil
	}
	a := s.cfg.State.Auth()
	if a == nil || a.TokenValue() == "" {
		return nil
	}
	return a
}

// roundQuota 配额刷新一轮：查余额并更新状态。
// 余额恢复的硬冷却（配额耗尽）自动解冻——每日重置后无需人工。
// quota_path 为空时查 Marvis 每日额度。没填 guid 的账号跳过。
func (s *Scheduler) roundQuota(ctx context.Context) {
	if s.cfg.Upstream == nil {
		return
	}
	a := s.current()
	if a == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	q, err := s.cfg.Upstream.Quota(cctx, a)
	if err != nil {
		if err != upstream.ErrUnsupported && err != upstream.ErrNoGUID {
			log.Printf("WARN: [scheduler] quota err=%v", err)
		}
		return
	}
	s.cfg.State.SetQuota(q.Remaining, q.Total)
	s.cfg.State.UnfreezeQuota(q.Remaining)
	log.Printf("[scheduler] quota refresh: remaining=%d total=%d", q.Remaining, q.Total)
}

// roundKeepalive 保活一轮：用 refresh_token 换新的 access token 并落盘。
// 刷新失败只记日志（下个时点重试；401 类失败说明 token 已彻底失效，人工介入）。
func (s *Scheduler) roundKeepalive(ctx context.Context) {
	if s.cfg.Upstream == nil || s.cfg.Upstream.RefreshPath == "" {
		log.Printf("[scheduler] keepalive: refresh 端点未配置，跳过")
		return
	}
	a := s.current()
	if a == nil {
		log.Printf("[scheduler] keepalive: 未配置凭证，跳过")
		return
	}
	if a.RefreshTokenValue() == "" {
		log.Printf("[scheduler] keepalive: 没有 refresh_token，跳过")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	if err := s.cfg.Upstream.RefreshToken(cctx, a); err != nil {
		log.Printf("WARN: [scheduler] keepalive err=%v", err)
		return
	}
	if err := s.cfg.File.Save(a); err != nil {
		log.Printf("WARN: [scheduler] keepalive 新 token 落盘失败: %v", err)
		return
	}
	s.cfg.State.SetAuth(a)
	s.lastRefresh = time.Now()
	log.Printf("[scheduler] keepalive: token 已刷新并落盘（有效期 %d 秒）", a.ExpiresAt)
}

// roundProbe 健康探测一轮：唯一已鉴权的轻量探针（上游无 /v1/models 时
// ModelsLive 走 1-token 最小对话）。结果记账到状态（观测用；成功同时
// NoteSuccess 让软冷却提前解除——熔断的指数退避不会被探测绕过）。
func (s *Scheduler) roundProbe(ctx context.Context) {
	if s.cfg.Upstream == nil {
		return
	}
	a := s.current()
	if a == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	started := time.Now()
	_, err := s.cfg.Upstream.ModelsLive(cctx, a)
	d := time.Since(started)
	if err != nil {
		// 负缓存兜底形态（返回静态目录 + err）视为失败。
		s.cfg.State.SetProbeResult(false, d, "probe: "+err.Error())
		log.Printf("probe FAIL latency=%s err=%v", d.Round(time.Millisecond), err)
		return
	}
	s.cfg.State.SetProbeResult(true, d, "")
	s.cfg.State.NoteSuccess()
	log.Printf("probe OK latency=%s", d.Round(time.Millisecond))
}

// RoundQuota 手动触发一轮配额刷新（面板「刷新配额」按钮）。
func (s *Scheduler) RoundQuota(ctx context.Context) { s.roundQuota(ctx) }

// RoundProbe 手动触发一轮探测（面板按钮）。
func (s *Scheduler) RoundProbe(ctx context.Context) { s.roundProbe(ctx) }
