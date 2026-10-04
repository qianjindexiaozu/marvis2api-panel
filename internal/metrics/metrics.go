// Package metrics 进程内按模型聚合的请求统计（/v1/stats 数据源，社区面板口径）。
//
// 全部来自网关成功/失败记账路径的单一埋点；重启清零，POST /v1/stats/reset
// 手动清空。不落盘——长周期统计看用量页（usage.json 日聚合），这里只服务
// 「观察增量」的短周期运维场景，与 workbuddy2api 的 metrics.go 同思路。
package metrics

import (
	"sort"
	"sync"
	"time"
)

// ModelStat 单模型派生统计（JSON 形态对齐 workbuddy2api 的 ModelStatPayload 子集；
// cache/credit 维度 Marvis 上游无对应概念，不输出）。
type ModelStat struct {
	Model string `json:"model"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// Snapshot 聚合快照。
type Snapshot struct {
	Enabled   bool        `json:"enabled"`
	Since     time.Time   `json:"since"`
	Now       time.Time   `json:"now"`
	UptimeSec int64       `json:"uptime_sec"`
	Total     ModelStat   `json:"total"`
	Models    []ModelStat `json:"models"`
}

// modelMetrics 单模型原始计数。
type modelMetrics struct {
	requests   int64
	success    int64
	failed     int64
	streaming  int64
	prompt     int64
	completion int64
	latencyMS  int64
	lastSeen   time.Time
}

// acc 累加器（total 派生用，与 models 同口径）。
type acc struct {
	requests, success, failed, streaming int64
	prompt, completion, latencyMS        int64
}

func (a *acc) add(mm *modelMetrics) {
	a.requests += mm.requests
	a.success += mm.success
	a.failed += mm.failed
	a.streaming += mm.streaming
	a.prompt += mm.prompt
	a.completion += mm.completion
	a.latencyMS += mm.latencyMS
}

// Metrics 进程级聚合器。
type Metrics struct {
	mu      sync.Mutex
	since   time.Time
	byModel map[string]*modelMetrics
}

// New 构建聚合器（since = 构建时刻）。
func New() *Metrics {
	return &Metrics{since: time.Now(), byModel: map[string]*modelMetrics{}}
}

// global 进程级单例（网关与 /v1/stats handler 共用）。
var global = New()

// Global 返回进程级聚合器。
func Global() *Metrics { return global }

// Record 记一次请求。model 为空忽略；status 2xx 记成功、其余记失败；
// prompt/completion 为负（usage 缺失）不计入。
func (m *Metrics) Record(model string, stream bool, status int, latency time.Duration, prompt, completion int64) {
	if model == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	mm := m.byModel[model]
	if mm == nil {
		mm = &modelMetrics{}
		m.byModel[model] = mm
	}
	mm.requests++
	if status >= 200 && status < 300 {
		mm.success++
	} else {
		mm.failed++
	}
	if stream {
		mm.streaming++
	}
	if prompt > 0 {
		mm.prompt += prompt
	}
	if completion > 0 {
		mm.completion += completion
	}
	if latency > 0 {
		mm.latencyMS += latency.Milliseconds()
	}
	mm.lastSeen = time.Now()
}

// Reset 清空累计（since 同步重置）。
func (m *Metrics) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.since = time.Now()
	m.byModel = map[string]*modelMetrics{}
}

// GlobalReset 清空进程级聚合（POST /v1/stats/reset）。
func GlobalReset() { global.Reset() }

// SnapshotOf 生成当前聚合快照。models 按请求数降序（面板表格默认序）；
// total 由各模型累加得出（与 models 同口径，避免两处算法分叉）。
func (m *Metrics) SnapshotOf() Snapshot {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := Snapshot{
		Enabled:   true,
		Since:     m.since,
		Now:       now,
		UptimeSec: int64(now.Sub(m.since).Seconds()),
		Models:    make([]ModelStat, 0, len(m.byModel)),
	}
	var tot acc
	for name, mm := range m.byModel {
		out.Models = append(out.Models, derive(name, mm, m.since))
		tot.add(mm)
	}
	out.Total = ModelStat{
		Model:            "total",
		Requests:         tot.requests,
		Success:          tot.success,
		Failed:           tot.failed,
		Streaming:        tot.streaming,
		AvgLatencyMS:     avgMS(tot.latencyMS, tot.requests),
		PromptTokens:     tot.prompt,
		CompletionTokens: tot.completion,
		TotalTokens:      tot.prompt + tot.completion,
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Requests > out.Models[j].Requests })
	return out
}

// derive 派生单模型统计。since 为聚合起点（tok/s 分母）。
func derive(name string, mm *modelMetrics, since time.Time) ModelStat {
	return ModelStat{
		Model:            name,
		Requests:         mm.requests,
		Success:          mm.success,
		Failed:           mm.failed,
		Streaming:        mm.streaming,
		AvgLatencyMS:     avgMS(mm.latencyMS, mm.requests),
		TokensPerSec:     tps(mm.completion, time.Since(since)),
		PromptTokens:     mm.prompt,
		CompletionTokens: mm.completion,
		TotalTokens:      mm.prompt + mm.completion,
		LastSeen:         lastSeenPtr(mm.lastSeen),
	}
}

func avgMS(sumMS, n int64) float64 {
	if n <= 0 {
		return 0
	}
	return float64(sumMS) / float64(n)
}

func tps(completion int64, span time.Duration) float64 {
	if span <= 0 {
		return 0
	}
	return float64(completion) / span.Seconds()
}

func lastSeenPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	cp := t
	return &cp
}
