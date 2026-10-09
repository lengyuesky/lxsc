package music

import (
	"context"
	"time"
)

// loadBoardDetached 不等待排队；满载时留给下一次目录请求重试，旧快照仍立即返回。
func (c *Catalog) loadBoardDetached(source, id string) {
	key := fullBoardKey(source, id)
	c.boardRefreshMu.Lock()
	if c.boardWarmClosed || c.boardRefreshing[key] {
		c.boardRefreshMu.Unlock()
		return
	}
	for key, at := range c.boardRefreshState {
		if time.Since(at) >= boardRefreshFailCooldown {
			delete(c.boardRefreshState, key)
		}
	}
	if _, ok := c.boardRefreshState[key]; ok {
		c.boardRefreshMu.Unlock()
		return
	}
	permit, err := c.boardWarmGate.Reserve(0)
	if err != nil {
		c.boardRefreshMu.Unlock()
		return
	}
	c.boardRefreshing[key] = true
	c.boardWarmWG.Add(1)
	ctx, cancel := context.WithTimeout(c.boardWarmCtx, boardLoadBudget)
	c.boardRefreshMu.Unlock()
	go func() {
		defer c.boardWarmWG.Done()
		defer cancel()
		defer permit.Release()
		defer func() {
			if recovered := recover(); recovered != nil && c.Log != nil {
				c.Log.Warn("榜单后台刷新异常", "source", source, "err", recovered)
			}
			c.boardRefreshMu.Lock()
			delete(c.boardRefreshing, key)
			c.boardRefreshMu.Unlock()
		}()
		if permit.Wait(ctx) != nil || c.waitForBoardIdle(ctx) != nil {
			return
		}
		cancel()
		// 读取预算由共享任务控制，实际完成前持续占用后台名额；服务退出统一取消。
		list, err := c.loadFullBoardMerged(c.boardWarmCtx, source, id, true)
		if err != nil && c.Log != nil {
			c.Log.Debug("榜单后台刷新失败", "source", source, "err", err)
		}
		c.boardRefreshMu.Lock()
		// 超时和空结果也冷却；排队拒绝、服务退出不留失败状态。
		if c.boardWarmCtx.Err() == nil && (err != nil || len(list) == 0) {
			c.boardRefreshState[key] = time.Now()
		}
		c.boardRefreshMu.Unlock()
	}()
}

// 后台任务在用户正在搜索或准备播放时让出启动机会；已开始的整榜继续完整读取。
func (c *Catalog) waitForBoardIdle(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		stats := c.RequestLimits.Stats()
		if stats.Active == 0 && stats.Queued == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// StopBoardWarm 在数据库和 SDK 关闭前取消排队、预热及尚未完成的整榜读取。
func (c *Catalog) StopBoardWarm() {
	if c == nil {
		return
	}
	c.boardRefreshMu.Lock()
	c.boardWarmClosed = true
	c.boardWarmEnabled = false
	if c.boardWarmCancel != nil {
		c.boardWarmCancel()
	}
	c.boardRefreshMu.Unlock()
	c.boardWarmWG.Wait()
	c.boardFlight.wait()
}
