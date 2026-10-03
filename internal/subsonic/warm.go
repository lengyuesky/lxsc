package subsonic

import (
	"context"
	"sync"
	"time"

	"lxsc/internal/music"
)

// boardWarmupSchedule 启动后的预热扫描时间点：SDK 与上游就绪后扫一遍，
// 稍后再扫一遍以重试瞬时失败；之后由列表请求触发的检查继续补齐。
var boardWarmupSchedule = []time.Duration{20 * time.Second, 6 * time.Minute}

// StartBoardWarmup 启用后台榜单预热并在启动后扫描可见榜单目录。
// 预热让客户端第一次点开榜单也直接命中快照，不再现场分页等待；
// 覆盖客户端不刷新歌单列表、直接点开榜单的场景。生产入口调用一次。
func (s *Server) StartBoardWarmup(ctx context.Context) {
	if s == nil || s.Catalog == nil {
		return
	}
	s.Catalog.EnableBoardWarm()
	go func() {
		for _, delay := range boardWarmupSchedule {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			s.warmVisibleBoardsOnce(ctx)
		}
	}()
}

// warmVisibleBoardsOnce 读取一次各平台可见榜单目录并交给后台预热；失败只记录日志。
func (s *Server) warmVisibleBoardsOnce(ctx context.Context) {
	if s.Settings == nil {
		return
	}
	display := newBoardVisibility(s.Settings.Get())
	if len(display.sources) == 0 {
		return
	}
	var mu sync.Mutex
	var visible []music.Board
	var wg sync.WaitGroup
	for _, source := range display.sources {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			boards, err := s.Catalog.Boards(callCtx, source)
			if err != nil {
				if s.Log != nil {
					s.Log.Warn("预热榜单目录失败", "source", source, "err", err)
				}
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, board := range boards {
				if display.allows(board) {
					visible = append(visible, board)
				}
			}
		}(source)
	}
	wg.Wait()
	s.Catalog.WarmVisibleBoards(visible)
}
