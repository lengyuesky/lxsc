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

const (
	// boardLoadBudget 整榜读取（含后台刷新）的时间预算。
	boardLoadBudget = 45 * time.Second
	// boardParallelPageLimit 已知总数时最多并行读取的页数，超出回退顺序读取。
	boardParallelPageLimit = 64
	// boardRefreshFailCooldown 后台刷新失败后的冷却时间，避免反复冲击故障上游。
	boardRefreshFailCooldown = time.Minute
	// boardPersistMaxAge 持久化快照的最长回放年龄，超过则重新完整读取。
	boardPersistMaxAge = 7 * 24 * time.Hour
)

// boardRefreshState 记录榜单后台刷新的失败冷却；成功刷新后快照自然新鲜。
type boardRefreshState struct {
	at time.Time
	ok bool
}

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
		c.refreshBoardDetached(source, id)
		return c.parseList(ctx, raw, source)
	}
	// 合并整个榜单的加载，避免多个客户端各自拼接分页。用户离开页面时
	// 已开始的整榜仍在预算内完成并缓存，下一次打开可以直接复用。
	return c.loadFullBoardMerged(ctx, source, id)
}

// boardSnapshot 返回可立即返回的完整快照：先内存后持久化，并回放到内存。
func (c *Catalog) boardSnapshot(source, id string) (json.RawMessage, bool) {
	key := fullBoardKey(source, id)
	if raw, ok := c.boardStale.Get(key); ok {
		return raw, true
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
	stale := json.RawMessage(raw)
	c.boardStale.Add(key, stale)
	return stale, true
}

// loadFullBoardMerged 经 singleflight 加载整榜；等待者取消不会中断读取。
// 成功的完整结果同时写入新鲜缓存、可回放缓存和数据库。
func (c *Catalog) loadFullBoardMerged(ctx context.Context, source, id string) ([]*Info, error) {
	key := fullBoardKey(source, id)
	raw, err := c.flight.do(ctx, key, func() (json.RawMessage, error) {
		if raw, ok := c.generic.Get(key); ok {
			return raw, nil
		}
		loadCtx, cancel := context.WithTimeout(context.Background(), boardLoadBudget)
		defer cancel()
		raw, err := c.loadFullBoard(loadCtx, source, id)
		if err == nil {
			c.generic.Add(key, raw)
			c.boardStale.Add(key, raw)
			c.persistBoardSnapshot(key, raw)
		}
		return raw, err
	})
	if err != nil {
		return nil, err
	}
	return c.parseList(ctx, raw, source)
}

// persistBoardSnapshot 尽力保存完整快照，失败只影响下次重启后的回放。
func (c *Catalog) persistBoardSnapshot(key string, raw json.RawMessage) {
	if c.DB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.DB.PutBoardSnapshot(ctx, key, raw); err != nil && c.Log != nil {
		c.Log.Debug("保存榜单快照失败", "err", err)
	}
}

// refreshBoardDetached 后台刷新一个榜单：与在途加载合并，失败记录短暂冷却；
// 冷却期间继续返回旧快照，不阻断客户端。
func (c *Catalog) refreshBoardDetached(source, id string) {
	key := fullBoardKey(source, id)
	c.boardRefreshMu.Lock()
	if c.boardRefreshing[key] {
		c.boardRefreshMu.Unlock()
		return
	}
	if state, ok := c.boardRefreshState[key]; ok && !state.ok && time.Since(state.at) < boardRefreshFailCooldown {
		c.boardRefreshMu.Unlock()
		return
	}
	c.boardRefreshing[key] = true
	c.boardRefreshMu.Unlock()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if c.Log != nil {
					c.Log.Warn("榜单后台刷新异常", "source", source, "err", recovered)
				}
			}
			c.boardRefreshMu.Lock()
			delete(c.boardRefreshing, key)
			c.boardRefreshMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), boardLoadBudget)
		defer cancel()
		_, err := c.loadFullBoardMerged(ctx, source, id)
		if err != nil && c.Log != nil {
			c.Log.Debug("榜单后台刷新失败", "source", source, "err", err)
		}
		c.boardRefreshMu.Lock()
		c.boardRefreshState[key] = boardRefreshState{at: time.Now(), ok: err == nil}
		c.boardRefreshMu.Unlock()
	}()
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
func (c *Catalog) loadFullBoard(ctx context.Context, source, id string) (json.RawMessage, error) {
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
	if pages := acc.parallelPages(); pages > 0 {
		if raw, err, done := c.loadFullBoardParallel(ctx, source, id, acc, pages); done {
			return raw, err
		}
	}
	return c.loadFullBoardSequential(ctx, source, id, acc, acc.nextPage())
}

// nextPage 已按页序合并完成后应继续读取的页码。
func (a *boardAccumulator) nextPage() int { return a.lastPage + 1 }

func (c *Catalog) loadFullBoardParallel(ctx context.Context, source, id string, acc *boardAccumulator, pages int) (json.RawMessage, error, bool) {
	fetched := make([]boardPage, pages+1)
	errs := make([]error, pages+1)
	var wg sync.WaitGroup
	for page := 2; page <= pages; page++ {
		wg.Add(1)
		go func(page int) {
			defer wg.Done()
			p, err := c.fetchBoardPage(ctx, source, id, page)
			fetched[page], errs[page] = p, err
		}(page)
	}
	wg.Wait()
	for page := 2; page <= pages; page++ {
		if errs[page] != nil {
			return nil, errs[page], true
		}
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
