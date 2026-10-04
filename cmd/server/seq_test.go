// seq_test.go 网关请求序号发生器：起始值、单调性与并发安全。
package main

import (
	"sync"
	"testing"
)

// TestGatewaySeqStartsAtOne 序号从 1 开始单调递增。
func TestGatewaySeqStartsAtOne(t *testing.T) {
	seq := gatewaySeq()
	for i := int64(1); i <= 3; i++ {
		if v := seq(); v != i {
			t.Fatalf("第 %d 次取号应得 %d，得到 %d", i, i, v)
		}
	}
}

// TestGatewaySeqConcurrent 并发取号不重复、个数正确。
func TestGatewaySeqConcurrent(t *testing.T) {
	seq := gatewaySeq()
	const goroutines = 8
	const per = 100
	var wg sync.WaitGroup
	got := make(chan int64, goroutines*per)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < per; j++ {
				got <- seq()
			}
		}()
	}
	wg.Wait()
	close(got)
	seen := map[int64]bool{}
	for v := range got {
		if seen[v] {
			t.Fatalf("序号重复: %d", v)
		}
		seen[v] = true
	}
	if len(seen) != goroutines*per {
		t.Fatalf("序号个数应为 %d，得到 %d", goroutines*per, len(seen))
	}
}
