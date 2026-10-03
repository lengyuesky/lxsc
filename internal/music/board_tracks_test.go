package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/settings"
)

func boardPageJSON(total, limit int, ids ...int) json.RawMessage {
	list := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		list = append(list, map[string]any{"songmid": fmt.Sprint(id), "name": "测试歌曲", "interval": "00:03"})
	}
	result := map[string]any{"list": list, "limit": limit}
	if total >= 0 {
		result["total"] = total
	}
	raw, _ := json.Marshal(result)
	return raw
}

func TestFullBoardHasNoPlaylistSizeCapAndReusesCompleteCache(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	var calls atomic.Int32
	const total = 2505
	c.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		calls.Add(1)
		if path != "kg.leaderboard.getList" {
			t.Errorf("意外请求 %s", path)
		}
		page := args[1].(int)
		ids := []int{}
		for i := (page-1)*100 + 1; i <= min(page*100, total); i++ {
			ids = append(ids, i)
		}
		return boardPageJSON(total, 100, ids...), nil
	})
	before, _ := c.DB.Statistics(context.Background())
	list, err := c.FullBoardTracks(context.Background(), "kg", "8888")
	if err != nil || len(list) != total || calls.Load() != 26 {
		t.Fatalf("整榜被截断: count=%d calls=%d err=%v", len(list), calls.Load(), err)
	}
	for i, track := range list {
		if track.Key() != fmt.Sprint(i+1) {
			t.Fatalf("排名顺序变化: %d %s", i, track.Key())
		}
	}
	n, duration, ok := c.CachedBoardSummary("kg", "8888")
	if !ok || n != total || duration != 3*total {
		t.Fatal(n, duration, ok)
	}
	again, err := c.FullBoardTracks(context.Background(), "kg", "8888")
	if err != nil || len(again) != total || calls.Load() != 26 {
		t.Fatal("完整缓存没有复用", err, calls.Load())
	}
	after, _ := c.DB.Statistics(context.Background())
	if before.Tracks != after.Tracks || before.Albums != after.Albums || before.Artists != after.Artists {
		t.Fatal("浏览榜单不能持久化歌曲资料")
	}
}

func TestFullBoardRejectsPartialAndRepeatedPages(t *testing.T) {
	for _, kind := range []string{"失败", "提前空页", "重复页", "总数变化", "页码错误", "异常结构"} {
		t.Run(kind, func(t *testing.T) {
			c := newDirectoryTestCatalog(t)
			c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
				if args[1].(int) == 1 {
					return boardPageJSON(3, 2, 1, 2), nil
				}
				switch kind {
				case "失败":
					return nil, errors.New("上游失败")
				case "提前空页":
					return boardPageJSON(3, 2), nil
				case "重复页":
					return boardPageJSON(3, 2, 1, 2), nil
				case "总数变化":
					return boardPageJSON(4, 2, 3), nil
				case "页码错误":
					return json.RawMessage(`{"list":[{"songmid":"3"}],"total":3,"limit":2,"page":1}`), nil
				default:
					return json.RawMessage(`{"error":"failed"}`), nil
				}
			})
			list, err := c.FullBoardTracks(context.Background(), "kw", "16")
			if err == nil || list != nil {
				t.Fatalf("部分榜单不能作为成功返回: %v %v", list, err)
			}
			if _, _, ok := c.CachedBoardSummary("kw", "16"); ok {
				t.Fatal("第一页数量不能冒充完整缓存")
			}
		})
	}
}

func TestFullBoardSupportsSDKPaginationVariants(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []json.RawMessage
		want  []string
	}{
		{"一次返回全部", []json.RawMessage{boardPageJSON(5, 2, 1, 2, 3, 4, 5)}, []string{"1", "2", "3", "4", "5"}},
		{"总数未知", []json.RawMessage{boardPageJSON(-1, 2, 1, 2), boardPageJSON(-1, 2, 3)}, []string{"1", "2", "3"}},
		{"过滤不可用歌曲", []json.RawMessage{boardPageJSON(4, 2, 1), boardPageJSON(4, 2, 3)}, []string{"1", "3"}},
		{"相邻页重复歌曲去重", []json.RawMessage{boardPageJSON(4, 2, 1, 2), boardPageJSON(4, 2, 2, 3)}, []string{"1", "2", "3"}},
		{"空榜单", []json.RawMessage{boardPageJSON(0, 100)}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newDirectoryTestCatalog(t)
			calls := 0
			c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
				calls++
				p := args[1].(int)
				if p > len(tc.pages) {
					return nil, errors.New("不应读取更多分页")
				}
				return tc.pages[p-1], nil
			})
			list, err := c.FullBoardTracks(context.Background(), "kg", "8888")
			if err != nil {
				t.Fatal(err)
			}
			keys := []string{}
			for _, in := range list {
				keys = append(keys, in.Key())
			}
			if !reflect.DeepEqual(keys, tc.want) || calls != len(tc.pages) {
				t.Fatal(keys, calls)
			}
		})
	}
}

func TestFullBoardRetryDoesNotReuseIncompletePages(t *testing.T) {
	for _, failure := range []string{"上游失败", "榜单变更"} {
		t.Run(failure, func(t *testing.T) {
			c := newDirectoryTestCatalog(t)
			attempt, calls := 1, 0
			c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
				calls++
				page := args[1].(int)
				if attempt == 1 {
					if page == 1 {
						return boardPageJSON(3, 2, 1, 2), nil
					}
					if failure == "上游失败" {
						return nil, errors.New("临时不可用")
					}
					return boardPageJSON(4, 2, 3, 4), nil
				}
				if page == 1 {
					return boardPageJSON(4, 2, 5, 6), nil
				}
				return boardPageJSON(4, 2, 7, 8), nil
			})
			if _, err := c.FullBoardTracks(context.Background(), "kg", "8888"); err == nil {
				t.Fatal("首次不完整读取应失败")
			}
			attempt = 2
			list, err := c.FullBoardTracks(context.Background(), "kg", "8888")
			if err != nil || len(list) != 4 {
				t.Fatalf("上游恢复后应重新读取完整榜单，不能继续复用失败或旧分页: count=%d err=%v", len(list), err)
			}
			if list[0].Key() != "5" || list[3].Key() != "8" || calls != 4 {
				t.Fatalf("重试混用了两次读取的分页: first=%s last=%s calls=%d", list[0].Key(), list[3].Key(), calls)
			}
		})
	}
}

func TestFullBoardFinishesForReopeningAfterCallerLeaves(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var calls atomic.Int32
	c.SetRemoteCallerForTest(func(ctx context.Context, _ string, args ...any) (json.RawMessage, error) {
		calls.Add(1)
		if args[1].(int) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return boardPageJSON(3, 2, 1, 2), nil
		}
		return boardPageJSON(3, 2, 3), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.FullBoardTracks(ctx, "kg", "8888"); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("离开页面应立即取消等待: %v", err)
	}
	once.Do(func() { close(release) })
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if count, _, ok := c.CachedBoardSummary("kg", "8888"); ok {
			if count != 3 {
				t.Fatalf("只能发布整榜缓存: %d", count)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("离开页面后整榜未继续完成，再次打开仍需重复等待")
		case <-tick.C:
		}
	}
	list, err := c.FullBoardTracks(context.Background(), "kg", "8888")
	if err != nil || len(list) != 3 || calls.Load() != 2 {
		t.Fatalf("重新打开应直接使用完整缓存: count=%d calls=%d err=%v", len(list), calls.Load(), err)
	}
}

func TestFullBoardServesStaleSnapshotAndRefreshesInBackground(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	var mu sync.Mutex
	version := 1
	c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
		mu.Lock()
		v := version
		mu.Unlock()
		return boardPageJSON(2, 2, v*100+1, v*100+2), nil
	})
	first, err := c.FullBoardTracks(context.Background(), "kw", "16")
	if err != nil || len(first) != 2 {
		t.Fatal(err, len(first))
	}
	if first[0].Key() != "101" {
		t.Fatal("初始版本不符", first[0].Key())
	}
	// 模拟 30 分钟过期：新鲜缓存失效，完整快照仍保留。
	key := fullBoardKey("kw", "16")
	c.generic.Remove(key)
	n, _, ok := c.CachedBoardSummary("kw", "16")
	if !ok || n != 2 {
		t.Fatal("过期摘要应回退完整快照", n, ok)
	}
	mu.Lock()
	version = 2
	mu.Unlock()
	stale, err := c.FullBoardTracks(context.Background(), "kw", "16")
	if err != nil || len(stale) != 2 {
		t.Fatalf("过期榜单应立即返回旧快照: %v %v", stale, err)
	}
	if stale[0].Key() != "101" {
		t.Fatal("返回的不是旧快照", stale[0].Key())
	}
	// 后台刷新完成后，快照替换为新版本。
	deadline := time.After(5 * time.Second)
	for {
		fresh, err := c.FullBoardTracks(context.Background(), "kw", "16")
		if err == nil && len(fresh) == 2 && fresh[0].Key() == "201" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("后台刷新没有替换过期快照", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestFullBoardStaleSnapshotSurvivesRefreshFailure(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	failing := false
	c.SetRemoteCallerForTest(func(_ context.Context, _ string, _ ...any) (json.RawMessage, error) {
		if failing {
			return nil, errors.New("上游故障")
		}
		return boardPageJSON(2, 2, 1, 2), nil
	})
	if _, err := c.FullBoardTracks(context.Background(), "wy", "3778678"); err != nil {
		t.Fatal(err)
	}
	key := fullBoardKey("wy", "3778678")
	c.generic.Remove(key)
	failing = true
	for range 3 {
		list, err := c.FullBoardTracks(context.Background(), "wy", "3778678")
		if err != nil || len(list) != 2 {
			t.Fatalf("刷新失败时旧快照必须继续可用: %v %v", list, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := c.generic.Get(key); ok {
		t.Fatal("失败的刷新不能发布新缓存")
	}
}

func TestFullBoardParallelPagesLoadConcurrently(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	const total, limit = 400, 100
	releases := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(total/limit - 1) // 第一页顺序先行，其余页必须在任一页完成前同时到达
	c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
		page := args[1].(int)
		if page > 1 {
			arrived.Done()
			select {
			case <-releases:
			case <-time.After(5 * time.Second):
				return nil, errors.New("分页未能并发读取")
			}
		}
		ids := []int{}
		for i := (page-1)*limit + 1; i <= page*limit; i++ {
			ids = append(ids, i)
		}
		return boardPageJSON(total, limit, ids...), nil
	})
	go func() {
		arrived.Wait()
		close(releases)
	}()
	list, err := c.FullBoardTracks(context.Background(), "kg", "8888")
	if err != nil || len(list) != total {
		t.Fatalf("分页应并发读取: count=%d err=%v", len(list), err)
	}
	for i, track := range list {
		if track.Key() != fmt.Sprint(i+1) {
			t.Fatal("并发读取后仍须保持排名顺序", i, track.Key())
		}
	}
}

func TestFullBoardServesPersistedSnapshotAfterRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	openCatalog := func(t *testing.T) (*Catalog, *db.DB) {
		database, err := db.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		store, err := settings.New(context.Background(), database)
		if err != nil {
			database.Close()
			t.Fatal(err)
		}
		return NewCatalog(database, nil, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil))), database
	}

	c, database := openCatalog(t)
	c.SetRemoteCallerForTest(func(_ context.Context, _ string, _ ...any) (json.RawMessage, error) {
		return boardPageJSON(2, 2, 1, 2), nil
	})
	first, err := c.FullBoardTracks(context.Background(), "wy", "3778678")
	if err != nil || len(first) != 2 || first[0].Key() != "1" {
		t.Fatalf("首次加载失败: %v %v", first, err)
	}
	database.Close()

	// 模拟重启：同一数据库上的新实例；后台刷新被阻塞时也必须立即回放持久化快照。
	restarted, database2 := openCatalog(t)
	block := make(chan struct{})
	t.Cleanup(func() { close(block); database2.Close() })
	restarted.SetRemoteCallerForTest(func(_ context.Context, _ string, _ ...any) (json.RawMessage, error) {
		<-block
		return boardPageJSON(2, 2, 9, 9), nil
	})
	loaded := make(chan []*Info, 1)
	go func() {
		list, err := restarted.FullBoardTracks(context.Background(), "wy", "3778678")
		if err != nil {
			t.Error(err)
		}
		loaded <- list
	}()
	select {
	case list := <-loaded:
		if len(list) != 2 || list[0].Key() != "1" || list[1].Key() != "2" {
			t.Fatalf("重启后应回放持久化快照: %v", list)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("重启后第一次打开被上游刷新阻塞，未立即回放持久化快照")
	}
}

func TestWarmVisibleBoardsPrefetchesMissingSnapshots(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	c.EnableBoardWarm()
	var mu sync.Mutex
	calls := map[string]int{}
	c.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		mu.Lock()
		calls[path]++
		mu.Unlock()
		return boardPageJSON(1, 100, 1), nil
	})
	// 已有可回放快照的榜单不重复预读：内存快照与持久化快照各一个。
	c.boardStale.Add(fullBoardKey("kg", "mem"), json.RawMessage(`{"list":[]}`))
	c.persistBoardSnapshot(fullBoardKey("tx", "db"), json.RawMessage(`{"list":[]}`))
	boards := []Board{{Source: "wy", BangID: "one"}, {Source: "kg", BangID: "mem"}, {Source: "tx", BangID: "db"}, {Source: "wy", BangID: "two"}}
	c.WarmVisibleBoards(boards)
	waitBoard := func(source, id string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			if _, _, ok := c.CachedBoardSummary(source, id); ok {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("榜单 %s|%s 未被预热", source, id)
			case <-time.After(time.Millisecond):
			}
		}
	}
	waitBoard("wy", "one")
	waitBoard("wy", "two")
	mu.Lock()
	loads := calls["wy.leaderboard.getList"] + calls["kg.leaderboard.getList"] + calls["tx.leaderboard.getList"]
	mu.Unlock()
	if loads != 2 {
		t.Fatalf("只应预读缺少快照的 2 个榜单: %v", calls)
	}
	// 重复预热不得重复请求。
	c.WarmVisibleBoards(boards)
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls["wy.leaderboard.getList"] != 2 {
		t.Fatalf("重复预热重复请求: %v", calls)
	}
}

func TestRoundRobinBoardsOrder(t *testing.T) {
	got := roundRobinBoards([]Board{{Source: "wy", BangID: "1"}, {Source: "wy", BangID: "2"}, {Source: "tx", BangID: "3"}, {Source: "kg", BangID: "4"}, {Source: "wy", BangID: "5"}})
	want := []string{"wy|1", "tx|3", "kg|4", "wy|2", "wy|5"}
	flat := make([]string, 0, len(got))
	for _, board := range got {
		flat = append(flat, board.Source+"|"+board.BangID)
	}
	if !reflect.DeepEqual(flat, want) {
		t.Fatalf("轮转顺序异常: %v", flat)
	}
}
