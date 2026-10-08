package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/admission"
)

func TestMetadataSearchCacheSeparatesKindsAndCopiesResults(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	ctx := context.Background()
	var calls atomic.Int32
	c.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		calls.Add(1)
		if args[0] != "分类" || args[1] != 1 || args[2] != 20 {
			t.Errorf("搜索参数错误: %v", args)
		}
		if strings.HasSuffix(path, "searchSinger") {
			return json.RawMessage(`{"list":[{"id":17,"name":"分类歌手","albumSize":3,"picUrl":"https://img.example/artist.jpg"}]}`), nil
		}
		return json.RawMessage(`{"list":[{"id":24,"name":"分类专辑","artistName":"分类歌手","artistId":17,"size":8,"picUrl":"https://img.example/album.jpg"}]}`), nil
	})
	opts := SearchOptions{Sources: []string{"wy"}, Limit: 20}
	artists, err := c.SearchArtists(ctx, "分类", opts)
	if err != nil || len(artists) != 1 || artists[0].ID != "17" || artists[0].AlbumSize != 3 {
		t.Fatal("原生歌手搜索失败", artists, err)
	}
	albums, err := c.SearchAlbums(ctx, "分类", opts)
	if err != nil || len(albums) != 1 || albums[0].ID != "24" || albums[0].ArtistID != "17" || albums[0].SongCount != 8 {
		t.Fatal("专辑原生字段未映射", albums, err)
	}
	artists[0].Name, albums[0].Name = "修改快照", "修改快照"
	artists, _ = c.SearchArtists(ctx, "分类", opts)
	albums, _ = c.SearchAlbums(ctx, "分类", opts)
	if calls.Load() != 2 || artists[0].Name != "分类歌手" || albums[0].Name != "分类专辑" {
		t.Fatal("缓存类型冲突或快照修改污染缓存", calls.Load(), artists, albums)
	}
	ref, ok := c.KnownArtistRef("wy", "分类歌手")
	if !ok || ref.ID != "17" || ref.Avatar == "" || ref.AlbumSize != 3 {
		t.Fatal("专辑摘要不能抹掉歌手头像或专辑数量", ref)
	}
	if _, err := c.Settings.Update(ctx, map[string]json.RawMessage{"searchCacheTTL": json.RawMessage(`0`)}); err != nil {
		t.Fatal(err)
	}
	c.RefreshTTL()
	for range 2 {
		_, _ = c.SearchArtists(ctx, "分类", opts)
	}
	if calls.Load() != 4 {
		t.Fatal("分类搜索未遵循搜索缓存设置")
	}
}

func TestMetadataSearchFallbackMatchesEntityAndSharesSongWork(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	// 只有一个工作名额时，分类回退也不能因嵌套占用名额而饿死。
	c.workLimits = admission.New(1, 8, 0, 0)
	c.search.gate, c.metadataSearch.gate = c.workLimits, c.workLimits
	var calls atomic.Int32
	c.searchCall = func(context.Context, string, ...any) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(`{"list":[
{"source":"kw","songmid":"one","name":"普通歌曲","singer":"测试歌手","singerId":123,"albumName":"测试专辑","albumId":"album"},
{"source":"kw","songmid":"two","name":"另一首","singer":"测试歌手","albumName":"测试专辑","albumId":"album"},
{"source":"kw","songmid":"two","name":"重复记录","singer":"测试歌手","albumName":"测试专辑","albumId":"album"},
{"source":"kw","songmid":"other","name":"测试歌曲","singer":"无关歌手","albumName":"其他专辑","albumId":"other"}
]}`), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var artists []ArtistRef
	var albums []AlbumMeta
	var artistErr, albumErr error
	var tasks sync.WaitGroup
	tasks.Go(func() {
		artists, artistErr = c.SearchArtists(ctx, "测试", SearchOptions{Sources: []string{"kw"}, Limit: 20})
	})
	tasks.Go(func() {
		albums, albumErr = c.SearchAlbums(ctx, "测试", SearchOptions{Sources: []string{"kw"}, Limit: 20})
	})
	tasks.Wait()
	if artistErr != nil || albumErr != nil || len(artists) != 1 || len(albums) != 1 || artists[0].Name != "测试歌手" || artists[0].AlbumSize != 1 || albums[0].SongCount != 2 {
		t.Fatal("回退应按实体名称匹配、去重并保留已知数量", artists, albums, artistErr, albumErr)
	}
	if calls.Load() != 1 {
		t.Fatal("同一平台两类回退搜索未共用歌曲请求", calls.Load())
	}
}

func TestMetadataSearchKeepsFastPlatformAndCancelsSlowWork(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	slowCanceled := make(chan struct{})
	c.SetRemoteCallerForTest(func(ctx context.Context, path string, _ ...any) (json.RawMessage, error) {
		if strings.HasPrefix(path, "wy.") {
			return json.RawMessage(`{"list":[{"id":"one","name":"可用歌手"}]}`), nil
		}
		<-ctx.Done()
		close(slowCanceled)
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	artists, err := c.SearchArtists(ctx, "可用", SearchOptions{Sources: []string{"wy", "tx"}})
	if err != nil || len(artists) != 1 || artists[0].Name != "可用歌手" {
		t.Fatal("慢平台不应清除已完成平台", artists, err)
	}
	select {
	case <-slowCanceled:
	case <-time.After(time.Second):
		t.Fatal("请求结束后慢平台仍在运行")
	}
}

func TestMetadataSearchFailureCanRetryAndBusyIsExplicit(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	var calls atomic.Int32
	c.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		switch calls.Add(1) {
		case 1:
			return nil, admission.ErrBusy
		case 2:
			return nil, fmt.Errorf("临时故障")
		default:
			return json.RawMessage(`{"list":[{"id":"one","name":"恢复的专辑","artistName":"歌手"}]}`), nil
		}
	})
	opts := SearchOptions{Sources: []string{"tx"}}
	if _, err := c.SearchAlbums(context.Background(), "恢复", opts); !errors.Is(err, admission.ErrBusy) {
		t.Fatal("繁忙应明确返回", err)
	}
	if _, err := c.SearchAlbums(context.Background(), "恢复", opts); err == nil {
		t.Fatal("平台故障不能返回成功空数组")
	}
	albums, err := c.SearchAlbums(context.Background(), "恢复", opts)
	if err != nil || len(albums) != 1 || calls.Load() != 3 {
		t.Fatal("失败被缓存，恢复后无法重试", albums, err)
	}
}
