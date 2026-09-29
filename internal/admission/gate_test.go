package admission

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLimitsFairnessAndCancellation(t *testing.T) {
	g := New(2, 3, 1, 1)
	a, _ := g.Reserve(1)
	a2, _ := g.Reserve(1)
	if _, err := g.Reserve(1); !errors.Is(err, ErrBusy) {
		t.Fatal("个人队列应有上限")
	}
	b, _ := g.Reserve(2)
	if g.Stats().Active != 2 {
		t.Fatal("已达个人限额的用户不能挡住其他用户")
	}
	c, _ := g.Reserve(3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(c.Wait(ctx), context.Canceled) {
		t.Fatal("取消排队应返回取消错误")
	}
	b.Release()
	if g.Stats().Queued != 1 {
		t.Fatal("不能把同用户排队任务提前启动")
	}
	a.Release()
	if err := a2.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2.Release()
	a2.Release()
	if g.Stats().Active != 0 || g.Stats().Queued != 0 || len(g.users) != 0 {
		t.Fatal("释放后不应残留额度和用户记录")
	}
}

func TestGlobalQueueAndConcurrentRelease(t *testing.T) {
	g := New(1, 1, 0, 0)
	a, _ := g.Reserve(1)
	b, _ := g.Reserve(2)
	if _, err := g.Reserve(3); !errors.Is(err, ErrBusy) {
		t.Fatal("全局队列满时必须立即拒绝")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(a.Release)
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	b.Release()
	if got := g.Stats(); got.Active != 0 || got.Queued != 0 || got.Rejected != 1 {
		t.Fatalf("额度计数错误: %+v", got)
	}
}
