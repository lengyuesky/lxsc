package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

func fullBoardKey(source, id string) string { return "board-full|" + source + "|" + id }

// boardSnapshotHasTracks 判断快照负载是否真的含歌曲。上游偶发返回空列表时，
// 空快照会被客户端当成空歌单长期缓存，因此空负载一律不作为可用快照。
func boardSnapshotHasTracks(raw json.RawMessage) bool {
	var result struct {
		List []map[string]any `json:"list"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return false
	}
	for _, item := range result.List {
		if item != nil {
			return true
		}
	}
	return false
}

const (
	// boardLoadBudget 整榜读取（含后台刷新）的时间预算。
	boardLoadBudget = 45 * time.Second
	// boardWarmConcurrency 后台预热与刷新榜单的并发上限，避免集中冲击上游。
	boardWarmConcurrency = 3
	// 后台先预占有限队列，不能为每个可见榜单创建一个无限等待的协程。
	boardWarmQueueLimit = 32
	// 前台整榜最多并行 4 页；后台逐页读取，给交互请求留下脚本 HTTP 容量。
	boardPageConcurrency = 4
	// boardParallelPageLimit 已知总数时最多并行读取的页数，超出回退顺序读取。
	boardParallelPageLimit = 64
	// boardRefreshFailCooldown 后台刷新失败后的冷却时间，避免反复冲击故障上游。
	boardRefreshFailCooldown = time.Minute
	// boardPersistMaxAge 持久化快照的最长回放年龄，超过则重新完整读取。
	boardPersistMaxAge = 7 * 24 * time.Hour
)

// FullBoardTracks 按平台分页读完榜单，不按固定歌曲数截断。
// 只有完整结果才能进入整榜缓存；过期的完整快照（内存或持久化）先返回、
// 后台再刷新，避免客户端在缓存过期或服务重启后的第一次打开长时间空白。
func (c *Catalog) FullBoardTracks(ctx context.Context, source, id string) ([]*Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !IsPlatform(source) || !validPlatformID(id) {
		return nil, errors.New("无效的榜单ID")
	}
	key := fullBoardKey(source, id)
	if raw, ok := c.generic.Get(key); ok {
		return c.parseList(ctx, raw, source)
	}
	if raw, ok := c.boardSnapshot(source, id); ok {
		// 仍持有完整旧快照：先让客户端立刻看到歌曲，后台刷新，失败保留旧快照。
		c.loadBoardDetached(source, id)
		return c.parseList(ctx, raw, source)
	}
	// 合并整个榜单的加载，避免多个客户端各自拼接分页。用户离开页面时
	// 已开始的整榜仍在预算内完成并缓存，下一次打开可以直接复用。
	return c.loadFullBoardMerged(ctx, source, id, false)
}

// boardSnapshot 返回可立即返回的完整快照：先内存后持久化，并回放到内存。
// 空负载既不回放也不返回：把"瞬时没有歌曲"当成榜单内容会让客户端一直开空页。
func (c *Catalog) boardSnapshot(source, id string) (json.RawMessage, bool) {
	key := fullBoardKey(source, id)
	if raw, ok := c.boardStale.Get(key); ok {
		if boardSnapshotHasTracks(raw) {
			return raw, true
		}
		c.boardStale.Remove(key)
	}
	if c.DB == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, at, err := c.DB.GetBoardSnapshot(ctx, key)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	if time.Since(time.UnixMilli(at)) > boardPersistMaxAge {
		return nil, false
	}
	if !boardSnapshotHasTracks(raw) {
		// 历史空快照：删掉，由打开或预热重新完整读取。
		c.deleteBoardSnapshots([]string{key})
		return nil, false
	}
	stale := json.RawMessage(raw)
	c.boardStale.Add(key, stale)
	return stale, true
}

// loadFullBoardMerged 经 singleflight 加载整榜；等待者取消不会中断读取。
// 成功的完整结果同时写入新鲜缓存、可回放缓存和数据库。
func (c *Catalog) loadFullBoardMerged(ctx context.Context, source, id string, background bool) ([]*Info, error) {
	// 停止后不能再注册共享任务；退出会先等待注册者，再等待实际加载。
	c.boardRefreshMu.Lock()
	if c.boardWarmClosed {
		c.boardRefreshMu.Unlock()
		return nil, context.Canceled
	}
	c.boardWarmWG.Add(1)
	c.boardRefreshMu.Unlock()
	defer c.boardWarmWG.Done()
	key := fullBoardKey(source, id)
	raw, err := c.boardFlight.doWithLifetime(ctx, c.boardWarmCtx, key, func() (json.RawMessage, error) {
		// 页面取消不打断整榜，但服务退出必须取消并等待实际加载结束。
		if err := c.boardWarmCtx.Err(); err != nil {
			return nil, err
		}
		loadCtx, cancel := context.WithTimeout(c.boardWarmCtx, boardLoadBudget)
		defer cancel()
		if raw, ok := c.generic.Get(key); ok {
			return raw, nil
		}
		concurrency := boardPageConcurrency
		if background {
			concurrency = 1
		}
		raw, err := c.loadFullBoard(loadCtx, source, id, concurrency)
		if err == nil {
			// 只缓存确实含歌曲的完整榜单：空结果进缓存后客户端会把它当成
			// 空歌单，且要等缓存过期才可能恢复。
			if boardSnapshotHasTracks(raw) {
				c.generic.Add(key, raw)
				c.boardStale.Add(key, raw)
				c.persistBoardSnapshot(key, raw)
			} else if c.Log != nil {
				c.Log.Warn("榜单没有歌曲，不缓存空结果", "source", source, "id", id)
			}
		}
		return raw, err
	})
	if err != nil {
		return nil, err
	}
	return c.parseList(ctx, raw, source)
}

// persistBoardSnapshot 尽力保存完整快照，失败只影响下次重启后的回放。
// 空榜单不落盘：重启后回放空快照同样会让客户端显示空歌单。
func (c *Catalog) persistBoardSnapshot(key string, raw json.RawMessage) {
	if c.DB == nil || !boardSnapshotHasTracks(raw) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.DB.PutBoardSnapshot(ctx, key, raw); err != nil && c.Log != nil {
		c.Log.Debug("保存榜单快照失败", "err", err)
	}
}

// boardPage 一次榜单分页的原始结果。
type boardPage struct {
	list  []map[string]any
	total *int
	limit int
	page  *int
}

func (c *Catalog) fetchBoardPage(ctx context.Context, source, id string, page int) (boardPage, error) {
	// 重试必须从新快照的第一页开始，不能混用此前失败时留下的旧页，
	// 也不能让某次临时失败阻断客户端随后重新打开榜单。
	raw, err := c.callRaw(ctx, fmt.Sprintf("board|%s|%s|%d", source, id, page), source+".leaderboard.getList", id, page)
	if err != nil {
		return boardPage{}, err
	}
	var result struct {
		List  []map[string]any `json:"list"`
		Total *int             `json:"total"`
		Limit int              `json:"limit"`
		Page  *int             `json:"page"`
	}
	if json.Unmarshal(raw, &result) != nil || result.List == nil || result.Limit < 0 || (result.Total != nil && *result.Total < 0) {
		return boardPage{}, errors.New("平台返回了无效的榜单分页")
	}
	return boardPage{list: result.List, total: result.Total, limit: result.Limit, page: result.Page}, nil
}

// boardAccumulator 按页序合并榜单分页，空页、重复页和失败不能伪装成完整榜单。
type boardAccumulator struct {
	source   string
	all      []map[string]any
	seen     map[string]bool
	expected int
	offset   int
	limit    int
	lastPage int
	complete bool
}

func newBoardAccumulator(source string) *boardAccumulator {
	return &boardAccumulator{source: source, all: make([]map[string]any, 0), seen: map[string]bool{}, expected: -1}
}

func (a *boardAccumulator) add(pageNo int, p boardPage) error {
	a.lastPage = pageNo
	if p.page != nil && *p.page != pageNo {
		return errors.New("平台返回了错误的榜单页码")
	}
	if pageNo == 1 && p.total != nil {
		a.expected = *p.total
		a.limit = p.limit
	}
	if pageNo > 1 && a.expected >= 0 && (p.total == nil || *p.total != a.expected) {
		return errors.New("榜单在读取期间发生变化，请重试")
	}
	if len(p.list) == 0 && a.expected > a.offset {
		return errors.New("平台提前返回空页，未取得完整榜单")
	}
	if a.expected == 0 && len(p.list) > 0 {
		return errors.New("榜单总数与歌曲数据不一致")
	}
	added := 0
	for _, item := range p.list {
		if item == nil {
			continue
		}
		if _, ok := item["source"]; !ok {
			item["source"] = a.source
		}
		in := FromMap(item)
		if in.Source() != a.source || !validPlatformID(in.Key()) {
			continue
		}
		if a.seen[in.TrackID()] {
			continue
		}
		a.seen[in.TrackID()] = true
		a.all = append(a.all, item)
		added++
	}
	if pageNo > 1 && len(p.list) > 0 && added == 0 {
		return errors.New("平台重复返回同一榜单页，未取得完整榜单")
	}
	// SDK 可能过滤不可用歌曲，因此按原始分页容量判断是否读到末页。
	step := max(p.limit, len(p.list))
	if step > int(^uint(0)>>1)-a.offset {
		return errors.New("榜单分页数量无效")
	}
	a.offset += step
	if a.expected < 0 {
		a.complete = p.limit == 0 || len(p.list) < p.limit
	} else {
		a.complete = a.offset >= a.expected
	}
	return nil
}

// parallelPages 返回已知总数时应并行读取的总页数；0 表示不能并行。
func (a *boardAccumulator) parallelPages() int {
	if a.complete || a.expected < 0 || a.limit <= 0 {
		return 0
	}
	pages := (a.expected + a.limit - 1) / a.limit
	if pages < 2 || pages > boardParallelPageLimit {
		return 0
	}
	return pages
}

func (a *boardAccumulator) marshal() (json.RawMessage, error) {
	return json.Marshal(map[string]any{"list": a.all})
}

// loadFullBoard 先读第一页并依据 total/limit 决定并行或顺序读取剩余页。
func (c *Catalog) loadFullBoard(ctx context.Context, source, id string, concurrency int) (json.RawMessage, error) {
	first, err := c.fetchBoardPage(ctx, source, id, 1)
	if err != nil {
		return nil, err
	}
	acc := newBoardAccumulator(source)
	if err := acc.add(1, first); err != nil {
		return nil, err
	}
	if acc.complete {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return acc.marshal()
	}
	if pages := acc.parallelPages(); pages > 0 && concurrency > 1 {
		if raw, err, done := c.loadFullBoardParallel(ctx, source, id, acc, pages, concurrency); done {
			return raw, err
		}
	}
	return c.loadFullBoardSequential(ctx, source, id, acc, acc.nextPage())
}

// nextPage 已按页序合并完成后应继续读取的页码。
func (a *boardAccumulator) nextPage() int { return a.lastPage + 1 }

func (c *Catalog) loadFullBoardParallel(ctx context.Context, source, id string, acc *boardAccumulator, pages, concurrency int) (json.RawMessage, error, bool) {
	fetched := make([]boardPage, pages+1)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var firstErr error
	var failed sync.Once
	var wg sync.WaitGroup
	for range min(concurrency, pages-1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for page := range jobs {
				p, err := c.fetchBoardPage(workCtx, source, id, page)
				if err != nil {
					failed.Do(func() { firstErr = err; cancel() })
					return
				}
				fetched[page] = p
			}
		}()
	}
enqueue:
	for page := 2; page <= pages; page++ {
		select {
		case jobs <- page:
		case <-workCtx.Done():
			break enqueue
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr, true
	}
	if err := ctx.Err(); err != nil {
		return nil, err, true
	}
	for page := 2; page <= pages; page++ {
		if err := acc.add(page, fetched[page]); err != nil {
			return nil, err, true
		}
		if acc.complete {
			break
		}
	}
	if acc.complete {
		if err := ctx.Err(); err != nil {
			return nil, err, true
		}
		raw, err := acc.marshal()
		return raw, err, true
	}
	// 并行页读满容量仍未完成（例如上游改小了每页数量）时回退顺序读取。
	return nil, nil, false
}

func (c *Catalog) loadFullBoardSequential(ctx context.Context, source, id string, acc *boardAccumulator, startPage int) (json.RawMessage, error) {
	for page := startPage; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := c.fetchBoardPage(ctx, source, id, page)
		if err != nil {
			return nil, err
		}
		if err := acc.add(page, p); err != nil {
			return nil, err
		}
		if acc.complete {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return acc.marshal()
		}
	}
}

// EnableBoardWarm 启用后台榜单预热。生产入口在启动时调用一次；
// 默认关闭让测试的上游调用计数保持确定。
func (c *Catalog) EnableBoardWarm() {
	if c == nil {
		return
	}
	c.boardRefreshMu.Lock()
	if !c.boardWarmClosed {
		c.boardWarmEnabled = true
	}
	c.boardRefreshMu.Unlock()
}

// WarmVisibleBoards 预读可见但尚无可用快照的榜单，让客户端第一次点开时直接命中快照。
// 按平台轮转排队，各平台靠前的榜单最先完成；新鲜缓存、可回放快照、在途加载
// 与失败冷却中的榜单都会跳过。只做后台加载，不阻塞调用方。
func (c *Catalog) WarmVisibleBoards(boards []Board) {
	if c == nil || len(boards) == 0 {
		return
	}
	c.boardRefreshMu.Lock()
	enabled := c.boardWarmEnabled
	c.boardRefreshMu.Unlock()
	if !enabled {
		return
	}
	persisted := c.persistedBoardKeys()
	for _, board := range roundRobinBoards(boards) {
		if !IsPlatform(board.Source) || !validPlatformID(board.BangID) {
			continue
		}
		key := fullBoardKey(board.Source, board.BangID)
		if _, ok := c.generic.Get(key); ok {
			continue
		}
		if _, ok := c.boardStale.Get(key); ok {
			continue
		}
		if persisted[key] {
			continue
		}
		c.loadBoardDetached(board.Source, board.BangID)
	}
}

// PruneInvalidBoardSnapshots 删除不含歌曲的持久化快照。历史版本会把上游瞬时的
// 空结果落盘，这类快照既不能回放，又会让预热跳过对应榜单，必须清掉后重建。
func (c *Catalog) PruneInvalidBoardSnapshots(ctx context.Context) int {
	if c == nil || c.DB == nil {
		return 0
	}
	keys, err := c.DB.BoardSnapshotKeys(ctx)
	if err != nil {
		return 0
	}
	var invalid []string
	for key := range keys {
		if ctx.Err() != nil {
			break
		}
		raw, _, err := c.DB.GetBoardSnapshot(ctx, key)
		if err != nil {
			continue
		}
		if !boardSnapshotHasTracks(raw) {
			invalid = append(invalid, key)
		}
	}
	c.deleteBoardSnapshots(invalid)
	return len(invalid)
}

// deleteBoardSnapshots 尽力删除快照；失败只影响下次预热是否重复读取。
func (c *Catalog) deleteBoardSnapshots(keys []string) {
	if c.DB == nil || len(keys) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.DB.DeleteBoardSnapshots(ctx, keys); err != nil && c.Log != nil {
		c.Log.Debug("删除无效榜单快照失败", "err", err)
	}
}

// persistedBoardKeys 返回 7 天内可回放的持久化快照键；读取失败时按无快照处理。
func (c *Catalog) persistedBoardKeys() map[string]bool {
	keys := map[string]bool{}
	if c.DB == nil {
		return keys
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.DB.BoardSnapshotKeys(ctx)
	if err != nil {
		return keys
	}
	for key, at := range rows {
		if time.Since(time.UnixMilli(at)) <= boardPersistMaxAge {
			keys[key] = true
		}
	}
	return keys
}

// roundRobinBoards 按平台轮转重排榜单，保证各平台靠前的榜单最先预热。
func roundRobinBoards(boards []Board) []Board {
	groups := map[string][]Board{}
	var order []string
	for _, board := range boards {
		if _, ok := groups[board.Source]; !ok {
			order = append(order, board.Source)
		}
		groups[board.Source] = append(groups[board.Source], board)
	}
	out := make([]Board, 0, len(boards))
	for index := 0; ; index++ {
		added := false
		for _, source := range order {
			if index < len(groups[source]) {
				out = append(out, groups[source][index])
				added = true
			}
		}
		if !added {
			return out
		}
	}
}
