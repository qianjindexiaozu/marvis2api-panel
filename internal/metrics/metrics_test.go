package metrics

import (
	"testing"
	"time"
)

func TestRecordAndSnapshot(t *testing.T) {
	m := New()
	m.Record("m1", true, 200, 1500*time.Millisecond, 100, 50)
	m.Record("m1", true, 200, 500*time.Millisecond, 10, 5)
	m.Record("m1", false, 502, 0, 0, 0)
	m.Record("m2", false, 200, 100*time.Millisecond, 3, 7)
	m.Record("", true, 200, time.Second, 1, 1) // 空 model 忽略

	snap := m.SnapshotOf()
	if !snap.Enabled || len(snap.Models) != 2 {
		t.Fatalf("应有 2 个模型: %+v", snap)
	}
	// 按请求数降序：m1(3) 在前。
	if snap.Models[0].Model != "m1" || snap.Models[1].Model != "m2" {
		t.Fatalf("排序不符: %v %v", snap.Models[0].Model, snap.Models[1].Model)
	}
	m1 := snap.Models[0]
	if m1.Requests != 3 || m1.Success != 2 || m1.Failed != 1 || m1.Streaming != 2 {
		t.Fatalf("m1 计数不符: %+v", m1)
	}
	if m1.PromptTokens != 110 || m1.CompletionTokens != 55 || m1.TotalTokens != 165 {
		t.Fatalf("m1 token 不符: %+v", m1)
	}
	// 平均时延按全部请求平摊（与 workbuddy2api 口径一致：未观测时延的请求计入分母）。
	if m1.AvgLatencyMS != 2000.0/3.0 {
		t.Fatalf("m1 平均时延应 ~666.7ms: %v", m1.AvgLatencyMS)
	}
	if m1.LastSeen == nil {
		t.Fatal("m1 应有 last_seen")
	}
	// total 与 models 同口径。
	if snap.Total.Requests != 4 || snap.Total.Success != 3 || snap.Total.Failed != 1 {
		t.Fatalf("total 计数不符: %+v", snap.Total)
	}
	if snap.Total.TotalTokens != 165+10 {
		t.Fatalf("total token 不符: %+v", snap.Total)
	}
}

func TestReset(t *testing.T) {
	m := New()
	m.Record("m1", true, 200, time.Second, 1, 1)
	m.Reset()
	snap := m.SnapshotOf()
	if len(snap.Models) != 0 || snap.Total.Requests != 0 {
		t.Fatalf("Reset 后应为空: %+v", snap)
	}
	if snap.Since.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("Reset 应刷新 since")
	}
}
