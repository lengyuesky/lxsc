package subsonic

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/admission"
)

func TestCoverSharedCancellationKeepsOtherWaiter(t *testing.T) {
	c := newCoverWork()
	gate := admission.New(2, 2, 1, 1)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	fetch := func(ctx context.Context) (coverImage, error) {
		calls.Add(1)
		started <- ctx
		select {
		case <-release:
			return coverImage{data: []byte("图片"), origin: "original"}, nil
		case <-ctx.Done():
			return coverImage{}, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := c.load(ctx, "same", 1, gate, fetch); first <- err }()
	workCtx := <-started
	go func() { _, _, err := c.load(context.Background(), "same", 1, gate, fetch); second <- err }()
	waitCoverCondition(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.waiters == 2 })
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) || workCtx.Err() != nil {
		t.Fatal("一个客户端取消中断了另一个等待者", err)
	}
	unblock()
	if err := <-second; err != nil || calls.Load() != 1 {
		t.Fatal("相同图片没有合并为一次上游请求", err, calls.Load())
	}
	if _, cached, err := c.load(context.Background(), "same", 1, gate, fetch); err != nil || !cached || calls.Load() != 1 {
		t.Fatal("共享成功结果未缓存", err)
	}
	if gate.Stats().Active != 0 || gate.Stats().Queued != 0 {
		t.Fatal("共享请求结束后名额未归还")
	}
}

func TestCoverAllWaitersCancelledAllowsFreshRetry(t *testing.T) {
	c := newCoverWork()
	gate := admission.New(1, 1, 1, 1)
	started := make(chan struct{})
	stopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := c.load(ctx, "same", 1, gate, func(ctx context.Context) (coverImage, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return coverImage{}, ctx.Err()
		})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("客户端取消未及时返回", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("所有等待者取消后，上游没有取消")
	}
	waitCoverCondition(t, func() bool { return gate.Stats().Active == 0 })
	image, cached, err := c.load(context.Background(), "same", 1, gate, func(context.Context) (coverImage, error) {
		return coverImage{data: []byte("新图片"), origin: "original"}, nil
	})
	if err != nil || cached || string(image.data) != "新图片" {
		t.Fatal("重试复用了已取消的请求", err)
	}
}

func TestCoverCacheUsesTotalByteBudgetAndExpiresStale(t *testing.T) {
	c := newCoverWork()
	c.mu.Lock()
	for _, key := range []string{"first", "second", "third"} {
		c.putLocked(key, coverImage{data: make([]byte, 6<<20), origin: "original"})
	}
	if c.bytes != 12<<20 || c.entries.Len() != 2 || c.entries.Contains("first") {
		t.Fatal("缓存没有按总字节预算淘汰最旧图片", c.bytes)
	}
	c.putLocked("second", coverImage{data: make([]byte, 8<<20), origin: "original"})
	if c.bytes != 14<<20 {
		t.Fatal("更新同一图片时重复累计了字节", c.bytes)
	}
	old, _ := c.entries.Peek("second")
	old.expires = time.Now().Add(-time.Minute)
	c.entries.Add("second", old)
	c.mu.Unlock()
	if _, ok := c.staleImage("second"); !ok {
		t.Fatal("近期成功的旧封面应可用于暂时降级")
	}
	c.mu.Lock()
	old.expires = time.Now().Add(-coverStaleTTL - time.Hour)
	c.entries.Add("second", old)
	c.mu.Unlock()
	if _, ok := c.staleImage("second"); ok {
		t.Fatal("过期太久的旧图不能继续兜底")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bytes != 6<<20 || c.bytes > coverCacheBytes {
		t.Fatal("移除旧图后缓存字节统计错误", c.bytes)
	}
}
