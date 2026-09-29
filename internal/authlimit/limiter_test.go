package authlimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSuccessfulAuthenticationDoesNotConsumeFailureBudget(t *testing.T) {
	var l Limiter
	now := time.Now()
	for range 1000 {
		finish, ok := l.Begin("alice", "127.0.0.1:123", now)
		if !ok {
			t.Fatal("正常请求不应按失败次数限制")
		}
		finish(true)
		finish(false)
	}
	if len(l.entries) != 0 {
		t.Fatal("正常请求不应留下无限键")
	}
	for range 20 {
		finish, ok := l.Begin("alice", "127.0.0.1:123", now)
		if !ok {
			t.Fatal("提前限频")
		}
		finish(false)
	}
	if _, ok := l.Begin("alice", "8.8.8.8:456", now); ok {
		t.Fatal("换IP不能绕过账户限频")
	}
	finish, ok := l.Begin("alice", "127.0.0.1:456", now.Add(time.Minute))
	if !ok {
		t.Fatal("窗口到期后必须恢复")
	}
	finish(true)
}

func TestConcurrentReservationsAndIPLimit(t *testing.T) {
	var l Limiter
	now := time.Now()
	var count atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			finish, ok := l.Begin("alice", "127.0.0.1:123", now)
			if ok {
				count.Add(1)
				finish(false)
			}
		})
	}
	wg.Wait()
	if count.Load() != 20 {
		t.Fatal("并发绕过失败预算", count.Load())
	}
	for i := range 40 {
		finish, ok := l.Begin(fmt.Sprint(i), "127.0.0.1:456", now)
		if !ok {
			t.Fatal("IP提前限频")
		}
		finish(false)
	}
	if _, ok := l.Begin("new", "127.0.0.1:789", now); ok {
		t.Fatal("变换用户名或端口不能绕过IP限频")
	}
}

func TestLimiterCapacityAndCleanup(t *testing.T) {
	var l Limiter
	now := time.Now()
	for i := range 2048 {
		finish, ok := l.Begin(fmt.Sprint(i), fmt.Sprintf("10.0.%d.%d", i/256, i%256), now)
		if !ok {
			t.Fatal("容量提前耗尽", i)
		}
		finish(false)
	}
	if len(l.entries) != 4096 {
		t.Fatal(len(l.entries))
	}
	if _, ok := l.Begin("new", "8.8.8.8", now); ok {
		t.Fatal("键数量必须有界")
	}
	finish, ok := l.Begin("new", "8.8.8.8", now.Add(time.Minute))
	if !ok {
		t.Fatal("过期项应清理")
	}
	finish(true)
	if len(l.entries) != 0 {
		t.Fatal("残留过期键")
	}
}
