package music

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/admission"
)

func TestRequestCacheBoundsUniqueWorkAndSharesFullQueue(t *testing.T) {
	c := newRequestCache[string, int](10, 0)
	c.gate = admission.New(1, 1, 0, 0)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fn := func(ctx context.Context) (int, bool, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return 1, false, nil
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	done := make(chan error, 3)
	go func() { _, err := c.load(context.Background(), "a", nil, time.Second, fn); done <- err }()
	<-started
	queuedCtx, cancel := context.WithCancel(context.Background())
	go func() { _, err := c.load(queuedCtx, "b", nil, time.Second, fn); done <- err }()
	waitCacheWaiters(t, c, 2)
	if _, err := c.load(context.Background(), "c", nil, time.Second, fn); !errors.Is(err, admission.ErrBusy) {
		t.Fatal("不同请求必须受全局队列上限限制")
	}
	go func() { _, err := c.load(context.Background(), "a", nil, time.Second, fn); done <- err }()
	waitCacheWaiters(t, c, 3)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("排队取消应及时退出")
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("共享任务只能执行一次，取消的排队任务不能执行: %d", calls.Load())
	}
}
