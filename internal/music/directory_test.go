package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/settings"
)

func newDirectoryTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	store, err := settings.New(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	return NewCatalog(database, nil, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestDirectorySingleflightMergesBoardRequests(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	var calls atomic.Int32
	catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		return json.RawMessage(`{"list":[{"id":"1","name":"榜单","bangid":"one"}]}`), nil
	})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			boards, err := catalog.Boards(context.Background(), "wy")
			if err != nil || len(boards) != 1 {
				t.Errorf("榜单结果异常: len=%d err=%v", len(boards), err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("重复榜单请求未被 singleflight 合并: %d", got)
	}
}

func TestSingleflightCallerCancellationDoesNotPoisonSharedRequest(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return json.RawMessage(`{"list":[{"name":"榜单","bangid":"one"}]}`), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := catalog.Boards(ctx, "wy")
		firstDone <- err
	}()
	<-started
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("首个等待者应按自身 context 取消: %v", err)
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := catalog.Boards(context.Background(), "wy")
		secondDone <- err
	}()
	close(release)
	if err := <-secondDone; err != nil {
		t.Fatalf("有效等待者不应被首个调用者取消拖累: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("共享上游请求应只执行一次: %d", calls.Load())
	}
}

func TestCanceledRemoteErrorIsNotNegativeCached(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	var calls atomic.Int32
	catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		if calls.Add(1) == 1 {
			return nil, context.Canceled
		}
		return json.RawMessage(`{"list":[{"name":"榜单","bangid":"one"}]}`), nil
	})
	if _, err := catalog.Boards(context.Background(), "wy"); !errors.Is(err, context.Canceled) {
		t.Fatalf("首次应返回取消错误: %v", err)
	}
	if boards, err := catalog.Boards(context.Background(), "wy"); err != nil || len(boards) != 1 {
		t.Fatalf("取消错误后应立即重试成功: len=%d err=%v", len(boards), err)
	}
	if calls.Load() != 2 {
		t.Fatalf("取消错误不应进入负缓存: %d", calls.Load())
	}
}

func TestAlbumSongsRestoresPersistedOnlineAlbumBeforeRemote(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	track := db.Track{ID: "tr-wy-one", Source: "wy", Name: "歌曲", Singer: "歌手", Album: "专辑", JSON: []byte(`{"source":"wy","songmid":"one","name":"歌曲","singer":"歌手","albumName":"专辑","albumId":"album-one"}`)}
	if err := database.UpsertTracks(ctx, []db.Track{track}); err != nil {
		t.Fatal(err)
	}
	locator := AlbumLocator{Source: "wy", ID: "album-one", Name: "专辑", Artist: "歌手", Parent: "sap-parent"}
	onlineID := OnlineAlbumID(locator.Source, locator.ID, locator.Name, locator.Artist, locator.Image, locator.Parent)
	ids, _ := json.Marshal([]string{track.ID})
	if err := database.UpsertAlbum(ctx, onlineID, "wy", "专辑", "歌手", ids); err != nil {
		t.Fatal(err)
	}
	store, _ := settings.New(ctx, database)
	catalog := NewCatalog(database, nil, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var calls atomic.Int32
	catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		calls.Add(1)
		return nil, errors.New("上游离线")
	})
	infos, meta, err := catalog.AlbumSongsFor(ctx, locator)
	if err != nil || len(infos) != 1 || meta.Name != "专辑" {
		t.Fatalf("重启后应从数据库恢复收藏专辑: len=%d meta=%+v err=%v", len(infos), meta, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("命中持久化收藏时不应请求上游: %d", calls.Load())
	}
}

func TestFallbackDoesNotPrefetchNextAlbumPage(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	var calls atomic.Int32
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path != "kw.musicSearch.search" {
			return nil, errors.New("意外接口")
		}
		calls.Add(1)
		list := make([]map[string]any, 0, 50)
		for index := 0; index < 50; index++ {
			list = append(list, map[string]any{"source": "kw", "songmid": fmt.Sprint(index), "name": "歌曲", "singer": "歌手", "albumName": fmt.Sprintf("专辑%d", index), "albumId": fmt.Sprintf("album-%d", index)})
		}
		data, _ := json.Marshal(map[string]any{"total": 100, "list": list})
		return data, nil
	})
	page, err := catalog.ArtistAlbums(context.Background(), ArtistRef{Source: "kw", Name: "歌手"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.List) != 50 || !page.HasMore {
		t.Fatalf("第一页结果异常: len=%d hasMore=%v", len(page.List), page.HasMore)
	}
	if calls.Load() != 1 {
		t.Fatalf("第一页不得预取下一批歌曲: %d", calls.Load())
	}
}

func TestLegacyAlbumUsesRemoteNameInsteadOfIDPlaceholder(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path != "wy.extendDetail.getAlbumSongs" {
			return nil, errors.New("意外接口")
		}
		return json.RawMessage(`{"name":"真实专辑名","list":[{"source":"wy","songmid":"one","name":"歌曲","singer":"歌手","albumName":"真实专辑名","albumId":"123"}]}`), nil
	})
	_, meta, err := catalog.AlbumSongs(context.Background(), "wy", "123")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "真实专辑名" {
		t.Fatalf("旧 al 深链应使用平台真实专辑名: %+v", meta)
	}
}

func TestFallbackSkipsAlbumsWithoutPlatformID(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path != "kw.musicSearch.search" {
			return nil, errors.New("意外接口")
		}
		return json.RawMessage(`{"total":2,"list":[{"source":"kw","songmid":"one","name":"一","singer":"歌手","albumName":"无ID专辑"},{"source":"kw","songmid":"two","name":"二","singer":"歌手","albumName":"正常专辑","albumId":"album-two"}]}`), nil
	})
	page, err := catalog.ArtistAlbums(context.Background(), ArtistRef{Source: "kw", Name: "歌手"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.List) != 1 || page.List[0].ID != "album-two" {
		t.Fatalf("不得输出无法打开的无 ID 专辑: %+v", page.List)
	}
}

func TestArtistDetailAcceptsNumericAndNestedPlatformIDs(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		switch path {
		case "wy.extendDetail.getArtistDetail":
			return json.RawMessage(`{"id":123,"name":"网易歌手","albumSize":7,"musicSize":20,"desc":"简介"}`), nil
		case "kg.singer.getInfo":
			return json.RawMessage(`{"id":456,"info":{"name":"酷狗歌手","avatar":"https://img.example/kg","desc":"简介"},"count":{"album":8,"music":30}}`), nil
		default:
			return nil, errors.New("意外接口")
		}
	})
	wy, err := catalog.ArtistDetail(context.Background(), ArtistRef{Source: "wy", ID: "123", Name: "网易歌手"})
	if err != nil || wy.ID != "123" || wy.AlbumSize != 7 || wy.Description != "简介" {
		t.Fatalf("网易歌手详情解析异常: %+v err=%v", wy, err)
	}
	kg, err := catalog.ArtistDetail(context.Background(), ArtistRef{Source: "kg", ID: "456", Name: "酷狗歌手"})
	if err != nil || kg.ID != "456" || kg.AlbumSize != 8 || kg.MusicSize != 30 {
		t.Fatalf("酷狗嵌套歌手详情解析异常: %+v err=%v", kg, err)
	}
}

func TestArtistSearchResponseMapsPlatformFields(t *testing.T) {
	catalog := newDirectoryTestCatalog(t)
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path != "tx.extendSearch.searchSinger" {
			return nil, nil
		}
		return json.RawMessage(`{"list":[{"id":"mid-1","name":"测试歌手","picUrl":"https://img.example/a.jpg","albumSize":66}]}`), nil
	})
	ref, err := catalog.ResolveArtist(context.Background(), ArtistRef{Source: "tx", Name: "测试歌手"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "mid-1" || ref.Name != "测试歌手" || ref.Avatar == "" || ref.AlbumSize != 66 {
		t.Fatalf("歌手搜索字段映射异常: %+v", ref)
	}
}

func TestVirtualDirectoryIDsRoundTripAndKeepLegacyIDs(t *testing.T) {
	name := "AC-DC / 测试歌手"
	singerID := SingerDirectoryID("tx", name, "mid-with-dash")
	ref, ok := ParseSingerDirectoryID(singerID)
	if !ok || ref.Source != "tx" || ref.Name != name || ref.ID != "mid-with-dash" {
		t.Fatalf("平台歌手 ID 往返失败: %+v ok=%v", ref, ok)
	}
	pageID := ArtistAlbumPageID("tx", name, ref.ID, 3)
	page, ok := ParseArtistAlbumPageID(pageID)
	if !ok || page.Page != 3 || page.Name != name || page.ID != ref.ID {
		t.Fatalf("专辑分页 ID 往返失败: %+v ok=%v", page, ok)
	}
	albumID := OnlineAlbumID("tx", "album-mid", "专辑-名", name, "https://img.example/1.jpg")
	album, ok := ParseOnlineAlbumID(albumID)
	if !ok || album.Source != "tx" || album.ID != "album-mid" || album.Name != "专辑-名" || album.Artist != name || album.Image == "" {
		t.Fatalf("在线专辑 ID 往返失败: %+v ok=%v", album, ok)
	}
	for _, legacy := range []string{"lb-wy-19723756", "al-tx-mid-with-dash", "ar-h-deadbeef"} {
		if _, ok := ParseID(legacy); !ok {
			t.Fatalf("旧 ID 不再可解析: %s", legacy)
		}
	}
}
