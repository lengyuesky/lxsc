package db

import (
	"context"
	"slices"
	"sync"
	"time"

	"lxsc/internal/admission"
)

type listeningQueryKey struct {
	userID  int64
	days    int
	day     string
	version uint64
}

type listeningCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	result  ListeningStats
	err     error
}

type listeningGroup struct {
	mu     sync.Mutex
	calls  map[listeningQueryKey]*listeningCall
	gate   *admission.Gate
	shared uint64
	closed bool
}

func (g *listeningGroup) init() {
	if g.calls == nil {
		g.calls = make(map[listeningQueryKey]*listeningCall)
		g.gate = admission.New(2, 16, 0, 0)
	}
}

// ListeningStatistics 只合并同版本、同日期、同用户与范围的在途查询，不缓存已完成结果。
// 一个页面离开不影响其他等待者，最后一个等待者取消时停止实际查询。
func (d *DB) ListeningStatistics(ctx context.Context, userID int64, days int, at time.Time) (ListeningStats, error) {
	if err := ctx.Err(); err != nil {
		return ListeningStats{}, err
	}
	if days != 7 && days != 30 && days != 90 && days != 365 {
		return ListeningStats{}, ErrListeningProgress
	}
	g := &d.listening
	g.mu.Lock()
	g.init()
	if g.closed {
		g.mu.Unlock()
		return ListeningStats{}, context.Canceled
	}
	key := listeningQueryKey{userID, days, at.In(ListeningZone).Format("2006-01-02"), d.listeningVersion.Load()}
	call := g.calls[key]
	if call == nil {
		permit, err := g.gate.Reserve(0)
		if err != nil {
			g.mu.Unlock()
			return ListeningStats{}, err
		}
		workCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		call = &listeningCall{done: make(chan struct{}), cancel: cancel}
		g.calls[key] = call
		go func() {
			defer cancel()
			var result ListeningStats
			err := permit.Wait(workCtx)
			if err == nil {
				result, err = d.listeningStatistics(workCtx, userID, days, at)
			}
			permit.Release()
			g.mu.Lock()
			call.result, call.err = result, err
			if g.calls[key] == call {
				delete(g.calls, key)
			}
			close(call.done)
			g.mu.Unlock()
		}()
	} else {
		g.shared++
	}
	call.waiters++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			call.cancel()
			if g.calls[key] == call {
				delete(g.calls, key)
			}
		}
		g.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return ListeningStats{}, ctx.Err()
	case <-call.done:
		if err := ctx.Err(); err != nil {
			return ListeningStats{}, err
		}
		result := call.result
		result.Daily = slices.Clone(result.Daily)
		result.TopTracks = slices.Clone(result.TopTracks)
		result.TopUsers = slices.Clone(result.TopUsers)
		return result, call.err
	}
}

// ListeningWorkload 只暴露数量，不包含用户身份与查询内容。
func (d *DB) ListeningWorkload() map[string]any {
	g := &d.listening
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	return map[string]any{"shared": g.shared, "inFlight": len(g.calls), "admission": g.gate.Stats()}
}

func (g *listeningGroup) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	for _, call := range g.calls {
		call.cancel()
	}
}
