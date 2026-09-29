package metrics

import (
	"errors"
	"sync"
	"testing"
)

func TestOperationConcurrentAccounting(t *testing.T) {
	var op Operation
	var wg sync.WaitGroup
	for i := range 100 {
		finish := op.Start()
		wg.Go(func() {
			if i%2 == 0 {
				finish(errors.New("失败"))
			} else {
				finish(nil)
			}
		})
	}
	wg.Wait()
	stats := op.Snapshot()
	var total int64
	for _, bucket := range stats.Buckets {
		total += bucket.Count
	}
	if stats.Active != 0 || stats.Count != 100 || stats.Failed != 50 || total != 100 {
		t.Fatalf("并发指标计数错误: %+v", stats)
	}
}
