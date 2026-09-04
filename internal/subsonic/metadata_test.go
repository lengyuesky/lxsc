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

func TestGeneratedMetadataTimeIsStable(t *testing.T) {
	info := music.FromMap(map[string]any{
		"source": "wy", "songmid": "1", "name": "歌曲", "singer": "歌手", "albumName": "专辑",
	})
	server := &Server{}
	first := server.songObj(nil, info)["created"]
	second := server.songObj(nil, info)["created"]
	if first != generatedMetadataTime || second != generatedMetadataTime {
		t.Fatalf("生成元数据时间不稳定: %v %v", first, second)
	}
}

func TestArtistPageUsesSeenTracksAndCapsAlbums(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
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
	var infos []*music.Info
	for i := 0; i < 40; i++ {
		infos = append(infos, music.FromMap(map[string]any{
			"source": "wy", "songmid": fmt.Sprintf("song-%d", i), "name": fmt.Sprintf("歌曲%d", i),
			"singer": "测试歌手", "albumId": fmt.Sprintf("album-%d", i), "albumName": fmt.Sprintf("专辑%d", i),
		}))
	}
	catalog.Cache(infos)
	artistID := music.ArtistID("测试歌手")
	catalog.CacheArtist(artistID, "测试歌手")
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log}
	req := httptest.NewRequest("GET", "/rest/getArtist.view?id="+artistID+"&f=json", nil)
	req = req.WithContext(withUser(req.Context(), user))
	recorder := httptest.NewRecorder()
	server.getArtist(recorder, req)

	var body map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	artist, ok := body["subsonic-response"]["artist"].(map[string]any)
	if !ok {
		t.Fatalf("歌手响应异常: %s", recorder.Body.String())
	}
	albums, _ := artist["album"].([]any)
	if len(albums) != store.Get().ArtistAlbumLimit {
		t.Fatalf("歌手页专辑数量未受限: %d", len(albums))
	}
	if count, _ := artist["albumCount"].(float64); int(count) != store.Get().ArtistAlbumLimit {
		t.Fatalf("歌手页 albumCount 异常: %v", artist["albumCount"])
	}
	if _, _, _, err := database.GetArtist(ctx, artistID); err == nil {
		t.Fatal("普通歌手浏览不应持久化歌手缓存")
	}
}

func TestGetPlaylistsExposesBoardsWithoutTrackScan(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.CreateUser(ctx, "user", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, map[string]json.RawMessage{
		"boardSources": json.RawMessage(`["wy"]`),
		"boardLimit":   json.RawMessage(`1`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePlaylistFull(ctx, "pl-real", user.ID, "真实歌单", "", false, nil); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	var paths []string
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		paths = append(paths, path)
		if path != "wy.leaderboard.getBoards" {
			return nil, fmt.Errorf("getPlaylists 不应请求歌曲接口: %s", path)
		}
		return json.RawMessage(`{"list":[{"id":"wy__one","name":"榜单一","bangid":"one"},{"id":"wy__two","name":"榜单二","bangid":"two"},{"id":"wy__three","name":"榜单三","bangid":"three"}]}`), nil
	})
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log}
	before, err := database.Statistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := func() []map[string]any {
		req := httptest.NewRequest("GET", "/rest/getPlaylists.view?f=json", nil)
		req = req.WithContext(withUser(req.Context(), user))
		recorder := httptest.NewRecorder()
		server.getPlaylists(recorder, req)
		var body map[string]map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		root := body["subsonic-response"]
		playlists, ok := root["playlists"].(map[string]any)
		if !ok {
			t.Fatalf("歌单响应异常: %+v", root)
		}
		items, ok := playlists["playlist"].([]any)
		if !ok {
			t.Fatalf("歌单列表格式异常: %+v", playlists)
		}
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			out = append(out, item.(map[string]any))
		}
		return out
	}
	items := request()
	if len(items) != 4 {
		t.Fatalf("应包含 1 个真实歌单和全部 3 个榜单，实际 %d: %+v", len(items), items)
	}
	names := map[string]bool{}
	for _, item := range items {
		names[item["name"].(string)] = true
	}
	for _, name := range []string{"真实歌单", "网易云 · 榜单一", "网易云 · 榜单二", "网易云 · 榜单三"} {
		if !names[name] {
			t.Fatalf("缺少歌单项 %q: %+v", name, names)
		}
	}
	if len(paths) != 1 || paths[0] != "wy.leaderboard.getBoards" {
		t.Fatalf("首次 getPlaylists 请求异常: %v", paths)
	}
	_ = request()
	if len(paths) != 1 {
		t.Fatalf("榜单名称缓存未生效: %v", paths)
	}
	after, err := database.Statistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Tracks != before.Tracks || after.Albums != before.Albums || after.Artists != before.Artists {
		t.Fatalf("getPlaylists 不应写入元数据: before=%+v after=%+v", before, after)
	}
}

func TestGetPlaylistsKeepsOtherBoardsWhenSourceFails(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.CreateUser(ctx, "user", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(ctx, map[string]json.RawMessage{
		"boardSources": json.RawMessage(`["wy","tx"]`),
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	var mu sync.Mutex
	var paths []string
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		if path == "tx.leaderboard.getBoards" {
			return nil, fmt.Errorf("QQ 榜单暂不可用")
		}
		if path == "wy.leaderboard.getBoards" {
			return json.RawMessage(`{"list":[{"name":"可用榜单","bangid":"one"}]}`), nil
		}
		return nil, fmt.Errorf("意外请求: %s", path)
	})
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log}
	req := httptest.NewRequest("GET", "/rest/getPlaylists.view?f=json", nil)
	req = req.WithContext(withUser(req.Context(), user))
	recorder := httptest.NewRecorder()
	server.getPlaylists(recorder, req)
	var body map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	items := body["subsonic-response"]["playlists"].(map[string]any)["playlist"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "网易云 · 可用榜单" {
		t.Fatalf("单个平台失败不应隐藏其他榜单: %+v", items)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("应分别尝试两个平台的榜单名称接口: %v", paths)
	}
	for _, path := range paths {
		if path != "wy.leaderboard.getBoards" && path != "tx.leaderboard.getBoards" {
			t.Fatalf("不应在列表阶段请求榜单歌曲: %v", paths)
		}
	}
}

func TestRootDirectoryIsStaticAndShowsConfiguredPlatforms(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.CreateUser(ctx, "user", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Get().ShowBoards {
		t.Fatal("新安装默认应展示榜单目录")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	remoteCalls := 0
	catalog.SetRequestObserver(func(music.RemoteRequest) { remoteCalls++ })
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log}
	req := httptest.NewRequest("GET", "/rest/getMusicDirectory.view?id=1&f=json", nil)
	req = req.WithContext(withUser(req.Context(), user))
	recorder := httptest.NewRecorder()
	server.getMusicDirectory(recorder, req)

	var body map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	root := body["subsonic-response"]
	directory, ok := root["directory"].(map[string]any)
	if !ok {
		t.Fatalf("目录响应异常: %+v", root)
	}
	children, ok := directory["child"].([]any)
	if !ok || len(children) != len(store.Get().BoardSources)+1 {
		t.Fatalf("根目录应包含歌手入口和全部平台榜单入口: %+v", directory["child"])
	}
	if remoteCalls != 0 {
		t.Fatalf("打开根目录不应触发下级扫描，实际请求 %d 次", remoteCalls)
	}
}
