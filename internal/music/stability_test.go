package music

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/settings"
)

func newStabilityCatalog(t *testing.T) *Catalog {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store, err := settings.New(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	return NewCatalog(database, nil, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func stabilityTrack() *Info {
	return FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "测试歌曲", "types": []any{map[string]any{"type": "128k"}, map[string]any{"type": "320k"}}})
}

func TestSearchCoalescingAndKeys(t *testing.T) {
	c := newStabilityCatalog(t)
	var calls atomic.Int32
	release := make(chan struct{})
	c.searchCall = func(ctx context.Context, path string, args ...any) (json.RawMessage, error) {
		calls.Add(1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		return json.RawMessage(`{"list":[{"songmid":"one","name":"第一首"},{"songmid":"two","name":"第二首"}]}`), nil
	}
	opts := SearchOptions{Sources: []string{"wy", "tx"}, Page: 1, Limit: 2}
	results := make(chan []*Info, 20)
	for range 20 {
		go func() { results <- c.Search(context.Background(), " 歌曲 ", opts) }()
	}
	waitCacheWaiters(t, c.search, 20)
	close(release)
	for range 20 {
		result := <-results
		if len(result) != 4 || result[0].Source() != "wy" || result[1].Source() != "tx" || result[2].Name() != "第二首" {
			t.Fatalf("必须保留交错顺序: %v", result)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("应只访问每个平台一次: %d", calls.Load())
	}
	c.Search(context.Background(), "歌曲", opts)
	if calls.Load() != 2 {
		t.Fatal("规范化查询应命中缓存")
	}
	for _, options := range []SearchOptions{
		{Sources: []string{"tx", "wy"}, Page: 1, Limit: 2},
		{Sources: []string{"wy", "tx"}, Page: 2, Limit: 2},
		{Sources: []string{"wy", "tx"}, Page: 1, Limit: 3},
	} {
		c.Search(context.Background(), "歌曲", options)
	}
	if calls.Load() != 8 {
		t.Fatalf("不同顺序、页码、条数不得误合并: %d", calls.Load())
	}
	if _, err := c.DB.GetTrack(context.Background(), TrackID("wy", "one")); err == nil {
		t.Fatal("搜索仍不得持久化歌曲")
	}
}

func TestSearchPartialAndEmptyNotCached(t *testing.T) {
	for _, mode := range []string{"partial", "empty", "failed"} {
		t.Run(mode, func(t *testing.T) {
			c := newStabilityCatalog(t)
			var calls atomic.Int32
			c.searchCall = func(ctx context.Context, path string, args ...any) (json.RawMessage, error) {
				calls.Add(1)
				if mode == "failed" || mode == "partial" && path == "tx.musicSearch.search" {
					return nil, fmt.Errorf("平台不可用")
				}
				if mode == "empty" {
					return json.RawMessage(`{"list":[]}`), nil
				}
				return json.RawMessage(`{"list":[{"songmid":"one","name":"歌曲"}]}`), nil
			}
			for range 2 {
				got := c.Search(context.Background(), "歌曲", SearchOptions{Sources: []string{"wy", "tx"}})
				want := 0
				if mode == "partial" {
					want = 1
				}
				if len(got) != want {
					t.Fatalf("部分失败仍应返回成功结果: %d", len(got))
				}
			}
			if calls.Load() != 4 {
				t.Fatalf("失败、空及残缺结果不应缓存: %d", calls.Load())
			}
		})
	}
}

func TestPlaybackRefreshCoalescesAndProtectsNewResult(t *testing.T) {
	c := newStabilityCatalog(t)
	in := stabilityTrack()
	var calls atomic.Int32
	release := make(chan struct{})
	c.urlCall = func(ctx context.Context, platform string, info any, quality string) (*js.MusicURLResult, error) {
		n := calls.Add(1)
		if n == 2 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
			}
		}
		return &js.MusicURLResult{URL: "https://cdn.example/same-address", Quality: quality}, nil
	}
	old, err := c.ResolvePlaybackURL(context.Background(), in, "320k")
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan URLResolution, 20)
	errors := make(chan error, 20)
	for range 20 {
		go func() { r, err := c.RefreshPlaybackURL(context.Background(), in, old); results <- r; errors <- err }()
	}
	waitCacheWaiters(t, c.urls, 20)
	close(release)
	var fresh URLResolution
	for range 20 {
		fresh = <-results
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		if fresh.token == old.token {
			t.Fatal("即使 URL 相同，也应区分解析版本")
		}
	}
	late, err := c.RefreshPlaybackURL(context.Background(), in, old)
	if err != nil || late.token != fresh.token || calls.Load() != 2 {
		t.Fatal("迟到的失败不应再次刷新")
	}
	late.Result.URL = "被调用者修改"
	copy, err := c.ResolveURL(context.Background(), in, "320k")
	if err != nil || copy.URL != "https://cdn.example/same-address" {
		t.Fatal("返回值不应暴露可变缓存")
	}
	if _, err := c.ResolveURL(context.Background(), in, "128k"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatal("不同音质必须分开取链")
	}
	c.InvalidateURLs()
	if _, err := c.ResolveURL(context.Background(), in, "320k"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("音源变更必须使直链失效")
	}
}

func TestRefreshTTLIsSelective(t *testing.T) {
	c := newStabilityCatalog(t)
	ctx := context.Background()
	var searches, urls atomic.Int32
	c.searchCall = func(context.Context, string, ...any) (json.RawMessage, error) {
		searches.Add(1)
		return json.RawMessage(`{"list":[{"songmid":"one"}]}`), nil
	}
	c.urlCall = func(ctx context.Context, source string, info any, quality string) (*js.MusicURLResult, error) {
		urls.Add(1)
		return &js.MusicURLResult{URL: "https://cdn.example/track", Quality: quality}, nil
	}
	load := func() {
		c.Search(ctx, "歌曲", SearchOptions{Sources: []string{"wy"}})
		if _, err := c.ResolveURL(ctx, stabilityTrack(), "320k"); err != nil {
			t.Fatal(err)
		}
	}
	update := func(key, raw string) {
		if _, err := c.Settings.Update(ctx, map[string]json.RawMessage{key: json.RawMessage(raw)}); err != nil {
			t.Fatal(err)
		}
		c.RefreshTTL()
	}
	load()
	update("serverName", `"新名称"`)
	load()
	if searches.Load() != 1 || urls.Load() != 1 {
		t.Fatal("无关设置不应清空缓存")
	}
	update("urlCacheTTL", `604800`)
	load()
	if searches.Load() != 1 || urls.Load() != 1 {
		t.Fatal("相同 TTL 不应清空缓存")
	}
	update("urlCacheTTL", `0`)
	load()
	load()
	if searches.Load() != 1 || urls.Load() != 3 {
		t.Fatal("关闭直链缓存只能影响直链")
	}
	update("searchCacheTTL", `0`)
	load()
	load()
	if searches.Load() != 3 || urls.Load() != 5 {
		t.Fatal("TTL=0 不得读取旧缓存或写入新缓存")
	}
}

func TestConcurrentCacheCoverDoesNotMutateMetadata(t *testing.T) {
	c := newStabilityCatalog(t)
	client := &http.Client{}
	sdk, err := js.NewSDKPool(1, "", `function __sdk_call() { return 'https://img.example/cover.jpg' }`, client, client, c.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Close()
	c.SDK = sdk
	c.urlCall = func(ctx context.Context, source string, info any, quality string) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://cdn.example/track", Quality: quality}, nil
	}
	info := stabilityTrack()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if got := c.Cover(context.Background(), info); got != "https://img.example/cover.jpg" {
					t.Errorf("封面缓存错误: %s", got)
				}
				_ = info.JSON()
				if _, err := c.ResolveURL(context.Background(), info, "320k"); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if info.Img() != "" {
		t.Fatal("封面查询不得修改共享歌曲元数据")
	}
}

func TestConcurrentCacheSettingsAndRequests(t *testing.T) {
	c := newStabilityCatalog(t)
	c.searchCall = func(context.Context, string, ...any) (json.RawMessage, error) {
		return json.RawMessage(`{"list":[{"songmid":"one","singer":"歌手"}]}`), nil
	}
	c.urlCall = func(ctx context.Context, source string, info any, quality string) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://cdn.example/track", Quality: quality}, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if worker == 0 {
					ttl := []int{0, 86400, -1}[i%3]
					_, err := c.Settings.Update(context.Background(), map[string]json.RawMessage{"urlCacheTTL": json.RawMessage(fmt.Sprint(ttl)), "searchCacheTTL": json.RawMessage(fmt.Sprint(i % 3))})
					if err != nil {
						errs <- err
					}
					c.RefreshTTL()
				} else if worker == 1 {
					c.PurgeMetadataCaches()
					c.InvalidateURLs()
					c.RefreshTTL()
				} else {
					c.Search(context.Background(), "歌曲", SearchOptions{Sources: []string{"wy"}})
					if _, err := c.ResolveURL(context.Background(), stabilityTrack(), "320k"); err != nil {
						errs <- err
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
