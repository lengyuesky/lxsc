package subsonic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

type directoryTestServer struct {
	server   *Server
	database *db.DB
	store    *settings.Store
	user     *db.User
}

func newDirectoryTestServer(t *testing.T) directoryTestServer {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	user, err := database.CreateUser(ctx, "user", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	return directoryTestServer{server: &Server{DB: database, Catalog: catalog, Settings: store, Log: log}, database: database, store: store, user: user}
}

func (fixture directoryTestServer) directory(t *testing.T, id string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/rest/getMusicDirectory.view?id="+id+"&f=json", nil)
	req = req.WithContext(withUser(req.Context(), fixture.user))
	recorder := httptest.NewRecorder()
	fixture.server.getMusicDirectory(recorder, req)
	var body map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	root := body["subsonic-response"]
	if root["status"] != "ok" {
		t.Fatalf("目录请求失败: %s", recorder.Body.String())
	}
	directory, ok := root["directory"].(map[string]any)
	if !ok {
		t.Fatalf("目录响应异常: %s", recorder.Body.String())
	}
	return directory
}

func TestBoardDirectoryReturnsAllNamesWithoutTrackScan(t *testing.T) {
	fixture := newDirectoryTestServer(t)
	var mu sync.Mutex
	var paths []string
	fixture.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		if path != "wy.leaderboard.getBoards" {
			return nil, fmt.Errorf("不应请求子级接口: %s", path)
		}
		list := make([]map[string]any, 0, 8)
		for i := 0; i < 8; i++ {
			list = append(list, map[string]any{"id": fmt.Sprint(i), "name": fmt.Sprintf("榜单%d", i), "bangid": fmt.Sprintf("board-%d", i)})
		}
		data, _ := json.Marshal(map[string]any{"list": list})
		return data, nil
	})
	directory := fixture.directory(t, "dir-wy")
	children, _ := directory["child"].([]any)
	if len(children) != 8 {
		t.Fatalf("榜单目录仍受旧 BoardLimit=%d 截断: %d", fixture.store.Get().BoardLimit, len(children))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "wy.leaderboard.getBoards" {
		t.Fatalf("父目录触发了子级扫描: %v", paths)
	}
}

func TestArtistParentLayersDoNotRequestPlatforms(t *testing.T) {
	fixture := newDirectoryTestServer(t)
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "歌曲", "singer": "测试歌手", "singerId": "123", "albumId": "a", "albumName": "专辑"})
	fixture.server.Catalog.Cache([]*music.Info{info})
	calls := 0
	fixture.server.Catalog.SetRequestObserver(func(music.RemoteRequest) { calls++ })
	fixture.directory(t, music.ArtistCategoryID("root"))
	fixture.directory(t, music.ArtistCategoryID("seen"))
	fixture.directory(t, music.ArtistNameID("seen", "测试歌手"))
	if calls != 0 {
		t.Fatalf("歌手父层请求不应提前扫描平台: %d", calls)
	}
}

func TestBoardSongsLoadOnlyWhenBoardIsOpened(t *testing.T) {
	fixture := newDirectoryTestServer(t)
	var paths []string
	fixture.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		paths = append(paths, path)
		switch path {
		case "wy.leaderboard.getBoards":
			return json.RawMessage(`{"list":[{"name":"完整榜单","bangid":"full"}]}`), nil
		case "wy.leaderboard.getList":
			return json.RawMessage(`{"list":[{"source":"wy","songmid":"one","name":"歌曲","singer":"歌手","albumName":"专辑","albumId":"a"}]}`), nil
		default:
			return nil, fmt.Errorf("意外接口: %s", path)
		}
	})
	fixture.directory(t, "dir-wy")
	if len(paths) != 1 || paths[0] != "wy.leaderboard.getBoards" {
		t.Fatalf("打开榜单名称目录时不应扫描歌曲: %v", paths)
	}
	fixture.directory(t, music.BoardID("wy", "full"))
	if len(paths) != 2 || paths[1] != "wy.leaderboard.getList" {
		t.Fatalf("只有打开具体榜单时才应请求歌曲: %v", paths)
	}
}

func TestOnlineAlbumLoadsSongsOnlyAfterOpenAndDoesNotPersist(t *testing.T) {
	fixture := newDirectoryTestServer(t)
	before, err := fixture.database.Statistics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	fixture.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		calls++
		if path != "wy.extendDetail.getAlbumSongs" || len(args) != 1 || fmt.Sprint(args[0]) != "album-one" {
			return nil, fmt.Errorf("意外专辑接口: %s %v", path, args)
		}
		return json.RawMessage(`{"list":[{"source":"wy","songmid":"one","name":"歌曲","singer":"歌手","albumName":"专辑","albumId":"album-one"}],"name":"专辑"}`), nil
	})
	albumID := music.OnlineAlbumID("wy", "album-one", "专辑", "歌手", "https://img.example/album.jpg")
	if calls != 0 {
		t.Fatalf("生成专辑节点不应请求歌曲: %d", calls)
	}
	directory := fixture.directory(t, albumID)
	children, _ := directory["child"].([]any)
	if len(children) != 1 || calls != 1 {
		t.Fatalf("打开专辑后应只请求一次歌曲: children=%d calls=%d", len(children), calls)
	}
	after, err := fixture.database.Statistics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Tracks != before.Tracks || after.Albums != before.Albums || after.Artists != before.Artists {
		t.Fatalf("普通在线专辑浏览不应落库: before=%+v after=%+v", before, after)
	}
}

func TestArtistDirectoryParentsAndOnlineAlbumIdentityStayConsistent(t *testing.T) {
	fixture := newDirectoryTestServer(t)
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "seed", "name": "种子", "singer": "测试歌手", "singerId": "123", "albumId": "seed-album", "albumName": "种子专辑"})
	fixture.server.Catalog.Cache([]*music.Info{info})
	fixture.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		switch path {
		case "wy.extendDetail.getArtistDetail":
			return json.RawMessage(`{"source":"wy","id":"123","name":"测试歌手","albumSize":60}`), nil
		case "wy.extendDetail.getArtistAlbums":
			return json.RawMessage(`{"total":60,"list":[{"id":"album-one","name":"专辑一","singer":"测试歌手","total":1}]}`), nil
		case "wy.extendDetail.getAlbumSongs":
			return json.RawMessage(`{"name":"专辑一","list":[{"source":"wy","songmid":"one","name":"歌曲一","singer":"测试歌手","singerId":"123","albumName":"专辑一","albumId":"album-one"}]}`), nil
		default:
			return nil, fmt.Errorf("意外接口: %s", path)
		}
	})
	nameID := music.ArtistNameID("seen", "测试歌手")
	nameDir := fixture.directory(t, nameID)
	platforms, _ := nameDir["child"].([]any)
	platform := platforms[0].(map[string]any)
	singerID := platform["id"].(string)
	if platform["parent"] != nameID {
		t.Fatalf("平台歌手节点 parent 异常: %+v", platform)
	}
	singerDir := fixture.directory(t, singerID)
	if singerDir["parent"] != nameID {
		t.Fatalf("歌手目录 parent 与父列表不一致: %+v", singerDir)
	}
	pages, _ := singerDir["child"].([]any)
	if len(pages) != 1 {
		t.Fatalf("歌手入口只应暴露第一页: %+v", pages)
	}
	pageID := pages[0].(map[string]any)["id"].(string)
	pageDir := fixture.directory(t, pageID)
	if pageDir["parent"] != singerID {
		t.Fatalf("第一页 parent 应为歌手平台节点: %+v", pageDir)
	}
	pageChildren, _ := pageDir["child"].([]any)
	albumNode := pageChildren[0].(map[string]any)
	albumID := albumNode["id"].(string)
	if albumNode["parent"] != pageID {
		t.Fatalf("专辑节点 parent 应为当前分页: %+v", albumNode)
	}
	albumDir := fixture.directory(t, albumID)
	if albumDir["parent"] != pageID {
		t.Fatalf("专辑目录 parent 与父列表不一致: %+v", albumDir)
	}
	songs, _ := albumDir["child"].([]any)
	song := songs[0].(map[string]any)
	if song["parent"] != albumID || song["albumId"] != albumID {
		t.Fatalf("在线专辑歌曲应沿用 oa 专辑身份: %+v", song)
	}
}

func TestArtistAlbumPageUsesFixedFiftyAndDoesNotPersist(t *testing.T) {
	fixture := newDirectoryTestServer(t)
	before, err := fixture.database.Statistics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var gotArgs []any
	fixture.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		if path != "wy.extendDetail.getArtistAlbums" {
			return nil, fmt.Errorf("意外接口: %s", path)
		}
		gotArgs = append([]any(nil), args...)
		list := make([]map[string]any, 0, 50)
		for i := 0; i < 50; i++ {
			list = append(list, map[string]any{"id": fmt.Sprintf("album-%d", i), "name": fmt.Sprintf("专辑%d", i), "singer": "测试歌手", "total": 10})
		}
		data, _ := json.Marshal(map[string]any{"list": list, "total": 120, "source": "wy"})
		return data, nil
	})
	pageID := music.ArtistAlbumPageID("wy", "测试歌手", "123", 2)
	directory := fixture.directory(t, pageID)
	children, _ := directory["child"].([]any)
	if len(children) != 51 {
		t.Fatalf("专辑页应包含 50 张专辑和下一页入口: %d", len(children))
	}
	if len(gotArgs) != 3 || fmt.Sprint(gotArgs[0]) != "123" || fmt.Sprint(gotArgs[1]) != "2" || fmt.Sprint(gotArgs[2]) != "50" {
		t.Fatalf("歌手专辑接口未使用固定 50 分页: %v", gotArgs)
	}
	after, err := fixture.database.Statistics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Tracks != before.Tracks {
		t.Fatalf("普通专辑目录浏览写入了 tracks: before=%d after=%d", before.Tracks, after.Tracks)
	}
	if _, _, _, _, err := fixture.database.GetAlbum(context.Background(), music.AlbumID("wy", "album-0", "", "")); err == nil {
		t.Fatal("普通专辑目录浏览不应写入 albums")
	}
	if _, _, _, err := fixture.database.GetArtist(context.Background(), music.ArtistID("测试歌手")); err == nil {
		t.Fatal("普通专辑目录浏览不应写入 artists")
	}
}

func TestNativeArtistDoesNotExpandOnline(t *testing.T) {
	fixture := newDirectoryTestServer(t)
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "歌曲", "singer": "测试歌手", "albumId": "a", "albumName": "专辑"})
	fixture.server.Catalog.Cache([]*music.Info{info})
	artistID := music.ArtistID("测试歌手")
	fixture.server.Catalog.CacheArtist(artistID, "测试歌手")
	calls := 0
	fixture.server.Catalog.SetRequestObserver(func(music.RemoteRequest) { calls++ })
	req := httptest.NewRequest("GET", "/rest/getArtist.view?id="+artistID+"&f=json", nil)
	req = req.WithContext(withUser(req.Context(), fixture.user))
	recorder := httptest.NewRecorder()
	fixture.server.getArtist(recorder, req)
	if calls != 0 {
		t.Fatalf("原生 getArtist 触发了在线扩展: %d", calls)
	}
}
