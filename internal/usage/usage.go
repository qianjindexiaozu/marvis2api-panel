// Package usage 逐请求用量记录：日聚合（落盘持久化）+ 最近请求环形缓冲（内存）。
//
// 精简口径：只记 account / model / token 用量 / 时长 / 状态，聚合到「天」粒度，
// 保留最近 90 天；最近 200 条明细仅在内存（重启清零，面板观测用）。
// 后台 30s 防抖落盘，用量数据与 state 文件同目录（usage.json）。
package usage

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record 单次请求的用量事实。
type Record struct {
	Time             time.Time `json:"time"`
	AccountID        string    `json:"account"`
	Model            string    `json:"model"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	DurationMs       int64     `json:"duration_ms"`
	Status           int       `json:"status"`
	Stream           bool      `json:"stream"`
}

// Totals 聚合子项（按账号 / 按模型 / 总计共用同一形态）。
type Totals struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// day 一天的聚合。
type day struct {
	Requests         int64              `json:"requests"`
	PromptTokens     int64              `json:"prompt_tokens"`
	CompletionTokens int64              `json:"completion_tokens"`
	ByAccount        map[string]*Totals `json:"by_account"`
	ByModel          map[string]*Totals `json:"by_model"`
}

func newDay() *day {
	return &day{ByAccount: map[string]*Totals{}, ByModel: map[string]*Totals{}}
}

// Recorder 用量记录器。
type Recorder struct {
	mu     sync.Mutex
	path   string
	days   map[string]*day // key: "2006-01-02"
	recent []Record        // 最近 200 条（时间升序追加，满则丢最旧）
	dirty  bool
	stopCh chan struct{}
}

// recentCap 最近请求明细的保留条数。
const recentCap = 200

// keepDays 日聚合保留天数（超出在落盘时淘汰）。
const keepDays = 90

// New 构建记录器并尝试从磁盘恢复（文件缺失/损坏静默按空表启动）。
func New(path string) *Recorder {
	r := &Recorder{path: path, days: map[string]*day{}}
	if raw, err := os.ReadFile(path); err == nil {
		var saved struct {
			Days map[string]*day `json:"days"`
		}
		if json.Unmarshal(raw, &saved) == nil && saved.Days != nil {
			r.days = saved.Days
		}
	}
	return r
}

// Start 启动后台防抖落盘。
func (r *Recorder) Start() {
	r.stopCh = make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-r.stopCh:
				return
			case <-t.C:
				r.Save()
			}
		}
	}()
}

// Stop 停止后台落盘并做最后一次落盘（幂等）。
func (r *Recorder) Stop() {
	if r.stopCh != nil {
		close(r.stopCh)
		r.stopCh = nil
	}
	r.Save()
}

// Record 记录一次请求。
func (r *Recorder) Record(rec Record) {
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	key := rec.Time.Format("2006-01-02")
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.days[key]
	if d == nil {
		d = newDay()
		r.days[key] = d
	}
	d.Requests++
	d.PromptTokens += rec.PromptTokens
	d.CompletionTokens += rec.CompletionTokens
	if ba := d.ByAccount[rec.AccountID]; ba != nil {
		ba.Requests++
		ba.PromptTokens += rec.PromptTokens
		ba.CompletionTokens += rec.CompletionTokens
	} else {
		d.ByAccount[rec.AccountID] = &Totals{Requests: 1, PromptTokens: rec.PromptTokens, CompletionTokens: rec.CompletionTokens}
	}
	if bm := d.ByModel[rec.Model]; bm != nil {
		bm.Requests++
		bm.PromptTokens += rec.PromptTokens
		bm.CompletionTokens += rec.CompletionTokens
	} else {
		d.ByModel[rec.Model] = &Totals{Requests: 1, PromptTokens: rec.PromptTokens, CompletionTokens: rec.CompletionTokens}
	}
	r.recent = append(r.recent, rec)
	if len(r.recent) > recentCap {
		r.recent = r.recent[len(r.recent)-recentCap:]
	}
	r.dirty = true
}

// Snapshot 面板聚合视图：hours 窗口（0 = 全部历史）。
// names 为账号 ID → 人读名称映射（仅展示用）。
func (r *Recorder) Snapshot(hours int, names map[string]string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Time{}
	if hours > 0 {
		cutoff = time.Now().Add(-time.Duration(hours) * time.Hour)
	}
	totals := &Totals{}
	byAccount := map[string]*Totals{}
	byModel := map[string]*Totals{}
	var daily []map[string]any
	keys := make([]string, 0, len(r.days))
	for k := range r.days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := r.days[k]
		dayStart, err := time.ParseInLocation("2006-01-02", k, time.Local)
		if err != nil {
			continue
		}
		inWindow := cutoff.IsZero() || dayStart.Add(24*time.Hour).After(cutoff)
		if inWindow {
			totals.Requests += d.Requests
			totals.PromptTokens += d.PromptTokens
			totals.CompletionTokens += d.CompletionTokens
			for id, a := range d.ByAccount {
				mergeAgg(byAccount, id, a)
			}
			for m, a := range d.ByModel {
				mergeAgg(byModel, m, a)
			}
		}
		// 日趋势序列只取窗口内的天（升序）。
		if inWindow && (hours > 0 || len(daily) < keepDays) {
			daily = append(daily, map[string]any{
				"date": k, "requests": d.Requests,
				"prompt_tokens": d.PromptTokens, "completion_tokens": d.CompletionTokens,
			})
		}
	}
	// 账号维度补人读名称。
	accounts := make([]map[string]any, 0, len(byAccount))
	ids := make([]string, 0, len(byAccount))
	for id := range byAccount {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := byAccount[id]
		name := id
		if n := names[id]; n != "" {
			name = n
		}
		accounts = append(accounts, map[string]any{
			"id": id, "name": name,
			"requests": a.Requests, "prompt_tokens": a.PromptTokens, "completion_tokens": a.CompletionTokens,
		})
	}
	models := make([]map[string]any, 0, len(byModel))
	mids := make([]string, 0, len(byModel))
	for m := range byModel {
		mids = append(mids, m)
	}
	sort.Strings(mids)
	for _, m := range mids {
		a := byModel[m]
		models = append(models, map[string]any{
			"model": m, "requests": a.Requests,
			"prompt_tokens": a.PromptTokens, "completion_tokens": a.CompletionTokens,
		})
	}
	recent := make([]Record, len(r.recent))
	copy(recent, r.recent)
	return map[string]any{
		"hours":      hours,
		"totals":     totals,
		"by_account": accounts,
		"by_model":   models,
		"daily":      daily,
		"recent":     recent,
	}
}

// Save 落盘（原子替换 + 90 天淘汰）。
func (r *Recorder) Save() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return
	}
	// 淘汰过期天。
	cutoff := time.Now().AddDate(0, 0, -keepDays).Format("2006-01-02")
	for k := range r.days {
		if k < cutoff {
			delete(r.days, k)
		}
	}
	out := struct {
		Version int             `json:"version"`
		SavedAt time.Time       `json:"saved_at"`
		Days    map[string]*day `json:"days"`
	}{Version: 1, SavedAt: time.Now(), Days: r.days}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("WARN: [usage] 落盘失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [usage] 落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		_ = os.Remove(tmp)
		log.Printf("WARN: [usage] 落盘失败: %v", err)
		return
	}
	r.dirty = false
}

func mergeAgg(dst map[string]*Totals, key string, a *Totals) {
	if x := dst[key]; x != nil {
		x.Requests += a.Requests
		x.PromptTokens += a.PromptTokens
		x.CompletionTokens += a.CompletionTokens
	} else {
		dst[key] = &Totals{Requests: a.Requests, PromptTokens: a.PromptTokens, CompletionTokens: a.CompletionTokens}
	}
}
