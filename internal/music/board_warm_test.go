package music

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/admission"
)

func TestBoardWarmQueueIsBoundedAndShutdownJoinsLoads(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	c.EnableBoardWarm()
	started := make(chan struct{}, boardWarmConcurrency)
	var active atomic.Int32
	c.SetRemoteCallerForTest(func(ctx context.Context, _ string, _ ...any) (json.RawMessage, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var boards []Board
	for i := range 100 {
		boards = append(boards, Board{Source: "wy", BangID: fmt.Sprint(i + 1)})
	}
	c.WarmVisibleBoards(boards)
	for range boardWarmConcurrency {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("后台工作未启动")
		}
	}
	stats := c.boardWarmGate.Stats()
	if stats.Active != boardWarmConcurrency || stats.Queued != boardWarmQueueLimit || stats.Rejected == 0 {
		t.Fatalf("预热队列没有限制容量: %+v", stats)
	}
	stopped := make(chan struct{})
	go func() { c.StopBoardWarm(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭没有取消并等待预热结束")
	}
	stats = c.boardWarmGate.Stats()
	if active.Load() != 0 || stats.Active != 0 || stats.Queued != 0 {
		t.Fatalf("关闭后仍有后台工作: active=%d stats=%+v", active.Load(), stats)
	}
	c.WarmVisibleBoards(boards)
	c.loadBoardDetached("wy", "after-close")
	c.boardRefreshMu.Lock()
	defer c.boardRefreshMu.Unlock()
	if len(c.boardRefreshing) != 0 {
		t.Fatal("关闭后仍接受任务或遗留在途标记")
	}
}

func TestBoardWarmYieldsToInteractiveRequests(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	permit, err := c.RequestLimits.Reserve(1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(permit.Release)
	started := make(chan struct{}, 1)
	c.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		started <- struct{}{}
		return boardPageJSON(1, 100, 1), nil
	})
	c.loadBoardDetached("wy", "one")
	select {
	case <-started:
		t.Fatal("用户请求尚在处理时启动了预热")
	case <-time.After(80 * time.Millisecond):
	}
	permit.Release()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("交互结束后预热没有恢复")
	}
}

func TestBoardShutdownReleasesSharedWorkQueue(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	c.boardFlight.gate = admission.New(1, 1, 0, 0)
	permit, err := c.boardFlight.gate.Reserve(0)
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	var calls atomic.Int32
	c.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		calls.Add(1)
		return boardPageJSON(1, 100, 1), nil
	})
	c.loadBoardDetached("wy", "queued")
	deadline := time.After(time.Second)
	for c.boardFlight.gate.Stats().Queued == 0 {
		select {
		case <-deadline:
			t.Fatal("榜单未进入共享任务队列")
		case <-time.After(time.Millisecond):
		}
	}
	c.StopBoardWarm()
	if calls.Load() != 0 || c.boardFlight.gate.Stats().Queued != 0 {
		t.Fatal("关闭后仍有等待共享额度的榜单任务")
	}
}

func TestRejectedBoardWarmCanBeRetried(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	c.boardWarmGate = admission.New(1, 0, 0, 0)
	permit, err := c.boardWarmGate.Reserve(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(permit.Release)
	called := make(chan struct{}, 1)
	c.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		called <- struct{}{}
		return boardPageJSON(1, 100, 1), nil
	})
	c.loadBoardDetached("wy", "retry")
	if c.boardWarmGate.Stats().Rejected != 1 {
		t.Fatal("队列满时应跳过本次预热")
	}
	permit.Release()
	c.loadBoardDetached("wy", "retry")
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("拒绝的预热遗留了在途或失败状态，无法重试")
	}
}

func TestFullBoardParallelismIsBoundedWithoutTruncation(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	const total = 30
	var active, peak atomic.Int32
	var ready sync.WaitGroup
	ready.Add(boardPageConcurrency)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	c.SetRemoteCallerForTest(func(ctx context.Context, _ string, args ...any) (json.RawMessage, error) {
		page := args[1].(int)
		if page > 1 {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			if page <= boardPageConcurrency+1 {
				ready.Done()
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return boardPageJSON(total, 1, page), nil
	})
	go func() { ready.Wait(); once.Do(func() { close(release) }) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tracks, err := c.FullBoardTracks(ctx, "wy", "large")
	if err != nil || len(tracks) != total || peak.Load() != boardPageConcurrency {
		t.Fatalf("分页并发或完整性异常: count=%d peak=%d err=%v", len(tracks), peak.Load(), err)
	}
	for i, track := range tracks {
		if track.Key() != fmt.Sprint(i+1) {
			t.Fatal("并发限制改变了榜单顺序")
		}
	}
}
