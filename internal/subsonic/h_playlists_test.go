package subsonic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

func TestVirtualPlaylistTimeIsStableAndOld(t *testing.T) {
	stamp, err := time.Parse(time.RFC3339, virtualPlaylistTime)
	if err != nil {
		t.Fatalf("虚拟榜单时间格式无效: %v", err)
	}
	if stamp.Year() >= 2020 {
		t.Fatalf("虚拟榜单时间应早于用户自建歌单，实际为 %s", stamp)
	}
}

func TestNativeBoardPlaylistLoadsSongsOnlyOnDetail(t *testing.T) {
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
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	var paths []string
	catalog.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		paths = append(paths, path)
		switch path {
		case "wy.leaderboard.getBoards":
			return json.RawMessage(`{"list":[{"id":"wy__one","name":"榜单一","bangid":"one"}]}`), nil
		case "wy.leaderboard.getList":
			if len(args) != 2 || fmt.Sprint(args[0]) != "one" || fmt.Sprint(args[1]) != "1" {
				return nil, fmt.Errorf("榜单参数异常: %v", args)
			}
			return json.RawMessage(`{"total":1,"list":[{"source":"wy","songmid":"one","name":"歌曲","singer":"歌手","albumName":"专辑","albumId":"album"}]}`), nil
		default:
			return nil, fmt.Errorf("意外接口: %s", path)
		}
	})
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log}
	request := func(handler func(http.ResponseWriter, *http.Request), path string) map[string]any {
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(withUser(req.Context(), user))
		recorder := httptest.NewRecorder()
		handler(recorder, req)
		var body map[string]map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		root := body["subsonic-response"]
		if root["status"] != "ok" {
			t.Fatalf("请求失败: %s", recorder.Body.String())
		}
		return root
	}
	before, err := database.Statistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := request(server.getPlaylists, "/rest/getPlaylists.view?f=json")
	playlists := root["playlists"].(map[string]any)["playlist"].([]any)
	if len(playlists) != 1 || playlists[0].(map[string]any)["name"] != "网易云 · 榜单一" {
		t.Fatalf("原生歌单未显示榜单摘要: %+v", playlists)
	}
	if len(paths) != 1 || paths[0] != "wy.leaderboard.getBoards" {
		t.Fatalf("getPlaylists 不应请求榜单歌曲: %v", paths)
	}
	root = request(server.getPlaylist, "/rest/getPlaylist.view?id="+music.BoardID("wy", "one")+"&f=json")
	playlist := root["playlist"].(map[string]any)
	if playlist["name"] != "网易云 · 榜单一" {
		t.Fatalf("榜单详情名称异常: %+v", playlist)
	}
	if entries, ok := playlist["entry"].([]any); !ok || len(entries) != 1 {
		t.Fatalf("榜单详情歌曲异常: %+v", playlist["entry"])
	}
	if len(paths) != 2 || paths[1] != "wy.leaderboard.getList" {
		t.Fatalf("打开榜单后请求路径异常: %v", paths)
	}
	_ = request(server.getPlaylist, "/rest/getPlaylist.view?id="+music.BoardID("wy", "one")+"&f=json")
	if len(paths) != 2 {
		t.Fatalf("榜单歌曲缓存未生效: %v", paths)
	}
	after, err := database.Statistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Tracks != before.Tracks || after.Albums != before.Albums || after.Artists != before.Artists {
		t.Fatalf("榜单歌单浏览不应持久化元数据: before=%+v after=%+v", before, after)
	}
}

func TestVirtualBoardPlaylistIsReadOnly(t *testing.T) {
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
	server := &Server{DB: database, Catalog: music.NewCatalog(database, nil, nil, store, slog.New(slog.NewTextHandler(io.Discard, nil))), Settings: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	boardID := music.BoardID("wy", "one")
	assertFailed := func(handler func(http.ResponseWriter, *http.Request), target string) {
		t.Helper()
		req := httptest.NewRequest("POST", target, nil)
		req = req.WithContext(withUser(req.Context(), user))
		recorder := httptest.NewRecorder()
		handler(recorder, req)
		var body map[string]map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["subsonic-response"]["status"] != "failed" {
			t.Fatalf("榜单歌单应为只读: %s", recorder.Body.String())
		}
	}
	assertFailed(server.createPlaylist, "/rest/createPlaylist.view?playlistId="+boardID+"&name=x&f=json")
	assertFailed(server.updatePlaylist, "/rest/updatePlaylist.view?playlistId="+boardID+"&name=x&f=json")
	assertFailed(server.deletePlaylist, "/rest/deletePlaylist.view?id="+boardID+"&f=json")
	if _, err := database.GetPlaylist(ctx, boardID); err == nil {
		t.Fatal("只读榜单不应创建数据库歌单")
	}
}

func TestGetPlaylistPermissions(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owner, _ := database.CreateUser(ctx, "owner", "enc", false, "320k")
	other, _ := database.CreateUser(ctx, "other", "enc", false, "320k")
	admin, _ := database.CreateUser(ctx, "admin", "enc", true, "320k")
	if err := database.CreatePlaylistFull(ctx, "pl-private", owner.ID, "私有歌单", "", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePlaylistFull(ctx, "pl-public", owner.ID, "公开歌单", "", true, nil); err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &Server{DB: database, Catalog: music.NewCatalog(database, nil, nil, store, log), Settings: store, Log: log}

	assertStatus := func(user *db.User, id, wantStatus string, wantCode int) {
		t.Helper()
		req := httptest.NewRequest("GET", "/rest/getPlaylist.view?id="+id+"&f=json", nil)
		req = req.WithContext(withUser(req.Context(), user))
		recorder := httptest.NewRecorder()
		server.getPlaylist(recorder, req)
		var body map[string]map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		root := body["subsonic-response"]
		if root["status"] != wantStatus {
			t.Fatalf("用户 %s 访问 %s 状态异常: %+v", user.Name, id, root)
		}
		if wantCode > 0 {
			errObj, _ := root["error"].(map[string]any)
			if int(errObj["code"].(float64)) != wantCode {
				t.Fatalf("错误码异常: %+v", root)
			}
		}
	}

	assertStatus(other, "pl-private", "failed", ErrNotAuthorized)
	assertStatus(owner, "pl-private", "ok", 0)
	assertStatus(admin, "pl-private", "ok", 0)
	assertStatus(other, "pl-public", "ok", 0)
}

func TestUpdatePlaylistAddsSongsToFront(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owner, _ := database.CreateUser(ctx, "owner", "enc", false, "320k")
	tracks := []db.Track{
		{ID: "tr-wy-old", Source: "wy", Name: "旧歌曲", JSON: []byte(`{"source":"wy","songmid":"old","name":"旧歌曲"}`)},
		{ID: "tr-wy-new1", Source: "wy", Name: "新歌曲一", JSON: []byte(`{"source":"wy","songmid":"new1","name":"新歌曲一"}`)},
		{ID: "tr-wy-new2", Source: "wy", Name: "新歌曲二", JSON: []byte(`{"source":"wy","songmid":"new2","name":"新歌曲二"}`)},
	}
	if err := database.UpsertTracks(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePlaylistFull(ctx, "pl-test", owner.ID, "测试", "", false, []string{"tr-wy-old"}); err != nil {
		t.Fatal(err)
	}
	store, _ := settings.New(ctx, database)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := &Server{DB: database, Catalog: music.NewCatalog(database, nil, nil, store, log), Settings: store, Log: log}
	req := httptest.NewRequest("POST", "/rest/updatePlaylist.view?playlistId=pl-test&songIdToAdd=tr-wy-new1&songIdToAdd=tr-wy-new2&f=json", nil)
	req = req.WithContext(withUser(req.Context(), owner))
	recorder := httptest.NewRecorder()
	server.updatePlaylist(recorder, req)

	playlist, err := database.GetPlaylist(ctx, "pl-test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tr-wy-new1", "tr-wy-new2", "tr-wy-old"}
	if len(playlist.TrackIDs) != len(want) {
		t.Fatalf("歌曲数量错误: %v", playlist.TrackIDs)
	}
	for i := range want {
		if playlist.TrackIDs[i] != want[i] {
			t.Fatalf("新增歌曲应位于歌单最前方: %v", playlist.TrackIDs)
		}
	}
}
