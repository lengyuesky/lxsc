package music

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitCacheWaiters[K comparable, V any](t *testing.T, c *requestCache[K, V], want int) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.mu.Lock()
		count := 0
		for _, call := range c.calls {
			count += call.waiters
		}
		c.mu.Unlock()
		if count == want {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("等待者数未达到 %d，当前 %d", want, count)
		case <-tick.C:
		}
	}
}

func TestRequestCacheTTLAndCapacity(t *testing.T) {
	for _, ttl := range []time.Duration{0, time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			c := newRequestCache[string, int](2, ttl)
			now := time.Unix(100, 0)
			c.now = func() time.Time { return now }
			var calls atomic.Int32
			fn := func(context.Context) (int, bool, error) { return int(calls.Add(1)), true, nil }
			load := func(key string) int {
				r, err := c.load(context.Background(), key, nil, time.Second, fn)
				if err != nil {
					t.Fatal(err)
				}
				return r.value
			}
			if load("a") != 1 {
				t.Fatal("第一次应访问上游")
			}
			if ttl == 0 {
				if load("a") != 2 || c.entries.Len() != 0 {
					t.Fatal("TTL=0 不得缓存")
				}
				return
			}
			if load("a") != 1 {
				t.Fatal("未过期应命中缓存")
			}
			now = now.Add(time.Second)
			if load("a") != 2 {
				t.Fatal("TTL=1 应按一秒过期")
			}
			load("b")
			load("c")
			if load("a") != 5 || c.entries.Len() != 2 {
				t.Fatal("缓存必须按固定容量淘汰")
			}
		})
	}
}

func TestRequestCacheCoalescesWithoutCaching(t *testing.T) {
	c := newRequestCache[string, int](2, 0)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var calls atomic.Int32
	fn := func(ctx context.Context) (int, bool, error) {
		calls.Add(1)
		select {
		case <-release:
			return 7, true, nil
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	results := make(chan error, 20)
	for range 20 {
		go func() {
			r, err := c.load(context.Background(), "a", nil, 5*time.Second, fn)
			if err == nil && r.value != 7 {
				err = errors.New("共享结果错误")
			}
			results <- err
		}()
	}
	waitCacheWaiters(t, c, 20)
	close(release)
	for range 20 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || c.entries.Len() != 0 {
		t.Fatalf("无缓存时仍应合并请求: %d", calls.Load())
	}
}

func TestRequestCacheIndependentCancellation(t *testing.T) {
	c := newRequestCache[string, int](2, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	remote := make(chan context.Context, 1)
	fn := func(ctx context.Context) (int, bool, error) {
		remote <- ctx
		select {
		case <-release:
			return 1, true, nil
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := c.load(ctx, "a", nil, 5*time.Second, fn); first <- err }()
	sharedCtx := <-remote
	go func() { _, err := c.load(context.Background(), "a", nil, 5*time.Second, fn); second <- err }()
	waitCacheWaiters(t, c, 2)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("等待者应独立取消: %v", err)
	}
	if sharedCtx.Err() != nil {
		t.Fatal("首个等待者不能取消其他人的上游任务")
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	waitCacheWaiters(t, c, 0)
}

func TestRequestCacheAbandonedCallCannotFillOrDeleteReplacement(t *testing.T) {
	c := newRequestCache[string, int](2, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote := make(chan context.Context, 1)
	oldRelease := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := c.load(ctx, "a", nil, time.Minute, func(ctx context.Context) (int, bool, error) {
			remote <- ctx
			<-oldRelease
			return 1, true, nil
		})
		result <- err
	}()
	sharedCtx := <-remote
	c.mu.Lock()
	oldDone := c.calls[flightKey[string]{key: "a"}].done
	c.mu.Unlock()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if sharedCtx.Err() == nil {
		t.Fatal("无人等待后必须取消上游")
	}
	waitCacheWaiters(t, c, 0)
	fresh, err := c.load(context.Background(), "a", nil, time.Second, func(context.Context) (int, bool, error) { return 2, true, nil })
	if err != nil || fresh.value != 2 {
		t.Fatalf("新任务失败: %v", err)
	}
	close(oldRelease)
	<-oldDone
	got, err := c.load(context.Background(), "a", nil, time.Second, func(context.Context) (int, bool, error) { return 3, true, nil })
	if err != nil || got.value != 2 {
		t.Fatalf("放弃的旧任务污染了新缓存: %v %v", got, err)
	}
}

func TestRequestCacheGenerationAndConditionalRefresh(t *testing.T) {
	c := newRequestCache[string, int](2, time.Minute)
	var calls atomic.Int32
	fn := func(context.Context) (int, bool, error) { return int(calls.Add(1)), true, nil }
	first, _ := c.load(context.Background(), "a", nil, time.Second, fn)
	c.configure(time.Minute)
	unchanged, _ := c.load(context.Background(), "a", nil, time.Second, fn)
	if unchanged.token != first.token {
		t.Fatal("相同 TTL 不应失效")
	}
	fresh, _ := c.load(context.Background(), "a", &first.token, time.Second, fn)
	late, _ := c.load(context.Background(), "a", &first.token, time.Second, fn)
	if fresh.value != 2 || late.token != fresh.token || calls.Load() != 2 {
		t.Fatal("迟到的失败不得移除已刷新的结果")
	}
	c.configure(2 * time.Minute)
	changed, _ := c.load(context.Background(), "a", nil, time.Second, fn)
	if changed.value != 3 || changed.token.generation == fresh.token.generation {
		t.Fatal("变更 TTL 必须切换代次")
	}
	c.purge()
	afterPurge, _ := c.load(context.Background(), "a", nil, time.Second, fn)
	if afterPurge.value != 4 {
		t.Fatal("主动清理必须失效")
	}
}

func TestRequestCacheOldGenerationCannotFill(t *testing.T) {
	for _, change := range []string{"ttl", "purge"} {
		t.Run(change, func(t *testing.T) {
			c := newRequestCache[string, int](2, time.Minute)
			started := make(chan struct{})
			release := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				_, err := c.load(context.Background(), "a", nil, 5*time.Second, func(context.Context) (int, bool, error) {
					close(started)
					<-release
					return 1, true, nil
				})
				result <- err
			}()
			<-started
			if change == "ttl" {
				c.configure(2 * time.Minute)
			} else {
				c.purge()
			}
			fresh, err := c.load(context.Background(), "a", nil, time.Second, func(context.Context) (int, bool, error) { return 2, true, nil })
			close(release)
			if oldErr := <-result; oldErr != nil || err != nil || fresh.value != 2 {
				t.Fatalf("新旧任务不应互相干扰: %v %v", oldErr, err)
			}
			got, _ := c.load(context.Background(), "a", nil, time.Second, func(context.Context) (int, bool, error) { return 3, true, nil })
			if got.value != 2 {
				t.Fatal("旧代次不能写回新缓存")
			}
		})
	}
}

func TestRequestCacheFailureAndTimeoutNotCached(t *testing.T) {
	c := newRequestCache[string, int](2, time.Minute)
	for _, fn := range []func(context.Context) (int, bool, error){
		func(context.Context) (int, bool, error) { return 0, false, errors.New("失败") },
		func(ctx context.Context) (int, bool, error) { <-ctx.Done(); return 0, false, ctx.Err() },
		func(context.Context) (int, bool, error) { panic("敏感错误不得透传") },
	} {
		if _, err := c.load(context.Background(), "a", nil, time.Millisecond, fn); err == nil {
			t.Fatal("失败必须返回错误")
		}
		if c.entries.Len() != 0 {
			t.Fatal("失败不得写入成功缓存")
		}
		waitCacheWaiters(t, c, 0)
	}
}
