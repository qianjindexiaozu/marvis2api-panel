// 观测快照：面板、/status、/healthz 的数据源（不含 token）。
package state

import (
	"encoding/json"
	"strings"
	"time"
)

// Status 单账号的观测快照（面板安全透出：只带 has_token 布尔）。
type Status struct {
	Name         string    `json:"name"`
	UID          string    `json:"uid,omitempty"`
	BaseURL      string    `json:"base_url,omitempty"`
	Configured   bool      `json:"configured"` // 是否已配置凭证
	State        string    `json:"state"`      // healthy / cooling / breaker / unconfigured / in_flight_full
	Reason       string    `json:"reason,omitempty"`
	Until        time.Time `json:"until,omitempty"`
	InFlight     int64     `json:"in_flight"`
	MaxInFlight  int       `json:"max_in_flight"`
	SuccessCount int64     `json:"success_count"`
	ErrTotal     int64     `json:"err_total"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitempty"`
	// 配额观测。
	QuotaRemaining int64     `json:"quota_remaining"`
	QuotaTotal     int64     `json:"quota_total"`
	QuotaUpdatedAt time.Time `json:"quota_updated_at,omitempty"`
	// 探测观测。
	LastProbeAt time.Time `json:"last_probe_at,omitempty"`
	LastProbeOK bool      `json:"last_probe_ok"`
	LatencyMs   int64     `json:"latency_ms"`
	HasToken    bool      `json:"has_token"`
	HasRefresh  bool      `json:"has_refresh"`
	Guid        string    `json:"guid,omitempty"`
	LoginType   string    `json:"login_type,omitempty"`
	Note        string    `json:"note,omitempty"`
	CreatedAt   string    `json:"created_at,omitempty"`
}

// MarshalJSON 不把零时间写成 0001-01-01。否则面板会把它当成一次已经发生的探活。
func (s Status) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(statusJSON(s))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for _, k := range []string{"until", "last_error_at", "quota_updated_at", "last_probe_at"} {
		v, _ := m[k].(string)
		if strings.HasPrefix(v, "0001-") {
			delete(m, k)
		}
	}
	return json.Marshal(m)
}

// statusJSON 避开 Status.MarshalJSON，否则编码会递归。
type statusJSON Status

// Status 返回当前观测快照。
func (s *State) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	st := Status{
		Configured:     s.a != nil,
		InFlight:       s.inFlight.Load(),
		MaxInFlight:    s.maxInFlight,
		SuccessCount:   s.successCount,
		ErrTotal:       s.errTotal,
		LastError:      s.lastError,
		LastErrorAt:    s.lastErrorAt,
		QuotaRemaining: s.quotaRemaining,
		QuotaTotal:     s.quotaTotal,
		QuotaUpdatedAt: s.quotaUpdatedAt,
		LastProbeAt:    s.lastProbeAt,
		LastProbeOK:    s.lastProbeOK,
		LatencyMs:      s.lastLatencyMs,
	}
	if s.a != nil {
		st.Name = s.a.Name
		st.UID = s.a.UID
		st.BaseURL = s.a.BaseURL
		st.Guid = s.a.Guid
		st.LoginType = s.a.LoginType
		st.Note = s.a.Note
		st.CreatedAt = s.a.CreatedAt
		st.HasToken = s.a.TokenValue() != ""
		st.HasRefresh = s.a.RefreshTokenValue() != ""
	}
	if !s.until.IsZero() && now.Before(s.until) {
		st.Until = s.until
	}
	// 熔断是独立的封禁时刻：比冷却晚时以熔断为准（面板倒计时口径）。
	if !s.breakerUntil.IsZero() && now.Before(s.breakerUntil) && (st.Until.IsZero() || s.breakerUntil.After(st.Until)) {
		st.Until = s.breakerUntil
	}
	st.Reason = s.reason
	switch {
	case s.a == nil || s.a.TokenValue() == "":
		st.State = "unconfigured"
	case !s.until.IsZero() && now.Before(s.until) && s.coolKind == CoolHard:
		st.State = "cooling"
	case !s.breakerUntil.IsZero() && now.Before(s.breakerUntil):
		st.State = "breaker"
	case !s.until.IsZero() && now.Before(s.until):
		st.State = "cooling"
	case s.inFlightFull():
		st.State = "in_flight_full"
	default:
		st.State = "healthy"
	}
	return st
}

// ReasonText 人读的不可用原因（reason 为空时按状态兜底）。
func (s Status) ReasonText() string {
	if s.Reason != "" {
		return s.Reason
	}
	switch s.State {
	case "unconfigured":
		return "未配置凭证"
	case "cooling":
		return "冷却中"
	case "breaker":
		return "熔断中"
	case "in_flight_full":
		return "在途请求占满"
	default:
		return "不可用"
	}
}

// Summary 汇总计数（/status、/healthz、面板总览用）。
// 单账号下 total 恒为 0 或 1，保留字段名以对齐家族约定。
func (s *State) Summary() (total, healthy int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total = 1
	if s.healthyLocked(time.Now()) {
		healthy = 1
	}
	return
}
