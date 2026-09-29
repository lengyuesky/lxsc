package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/admission"
)

func BenchmarkColdUniqueSearch(b *testing.B) {
	c := newStabilityCatalog(b)
	var sequence atomic.Uint64
	c.searchCall = func(ctx context.Context, _ string, _ ...any) (json.RawMessage, error) {
		timer := time.NewTimer(time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		return json.RawMessage(`{"list":[{"songmid":"one","name":"冷缓存歌曲"}]}`), nil
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if len(c.Search(context.Background(), fmt.Sprint(sequence.Add(1)), SearchOptions{Sources: []string{"wy", "tx"}})) != 2 {
				b.Error("冷缓存查询结果缺失")
			}
		}
	})
}

// TestSearchOverloadSoak 用环境变量显式运行持续压测，常规回归不等待一分钟。
func TestSearchOverloadSoak(t *testing.T) {
	if os.Getenv("LXSC_RUN_SOAK") != "1" {
		t.Skip("设置 LXSC_RUN_SOAK=1 运行六十秒压力测试")
	}
	c := newStabilityCatalog(t)
	c.search.gate = admission.New(4, 8, 0, 0)
	var sequence, completed, busy, expired atomic.Int64
	var active, peak atomic.Int64
	c.searchCall = func(ctx context.Context, _ string, args ...any) (json.RawMessage, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		if len(args) > 0 && len(args[0].(string)) > 0 && args[0].(string)[0] == 't' {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		timer := time.NewTimer(2 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		return json.RawMessage(`{"list":[{"songmid":"one","name":"压力测试"}]}`), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Go(func() {
			for ctx.Err() == nil {
				query := fmt.Sprint(sequence.Add(1))
				if worker%2 == 0 {
					query = "t" + query
				}
				requestCtx, stop := context.WithTimeout(ctx, 15*time.Millisecond)
				c.SearchProgress(requestCtx, query, SearchOptions{Sources: []string{"wy", "tx"}}, func(result SearchPlatformResult) {
					switch {
					case result.Err == nil:
						completed.Add(1)
					case errors.Is(result.Err, admission.ErrBusy):
						busy.Add(1)
					case errors.Is(result.Err, context.DeadlineExceeded):
						expired.Add(1)
					}
				})
				stop()
				time.Sleep(time.Millisecond)
			}
		})
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var maxHeap uint64
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-ticker.C:
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			if memory.HeapAlloc > maxHeap {
				maxHeap = memory.HeapAlloc
			}
			stats := c.search.gate.Stats()
			if stats.Active > 4 || stats.Queued > 8 {
				t.Errorf("超过容量: %+v", stats)
			}
		}
	}
	wg.Wait()
	deadline := time.Now().Add(time.Second)
	for c.search.gate.Stats().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := c.search.gate.Stats()
	if stats.Active != 0 || stats.Queued != 0 || active.Load() != 0 || peak.Load() > 4 || busy.Load() == 0 || expired.Load() == 0 {
		t.Fatalf("持续超时后额度泄漏: %+v, 上游=%d", stats, active.Load())
	}
	c.Search(context.Background(), "恢复验证", SearchOptions{Sources: []string{"wy"}})
	if completed.Load() == 0 {
		t.Fatal("过载期间没有成功请求")
	}
	t.Logf("六十秒: 查询=%d 成功平台=%d 繁忙平台=%d 超时平台=%d 上游峰值=%d 堆采样峰值=%.2f MiB 最终队列=%d", sequence.Load(), completed.Load(), busy.Load(), expired.Load(), peak.Load(), float64(maxHeap)/(1<<20), stats.Queued)
}
