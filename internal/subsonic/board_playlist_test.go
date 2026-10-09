package subsonic

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"lxsc/internal/music"
)

func TestBoardPlaylistVersionTracksContents(t *testing.T) {
	f := newDirectoryTestServer(t)
	req := httptest.NewRequest("GET", "/rest/getPlaylist?id=lb-wy-one&f=json", nil)
	rc := f.server.newReqCtx(req.WithContext(withUser(req.Context(), f.user)))
	board := music.Board{Source: "wy", BangID: "one", Name: "榜单"}
	one := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "歌曲一", "interval": "00:30"})
	two := music.FromMap(map[string]any{"source": "wy", "songmid": "two", "name": "歌曲二", "interval": "00:30"})
	build := func(infos []*music.Info) M {
		t.Helper()
		obj, err := f.server.boardPlaylistObj(rc, board, infos)
		if err != nil {
			t.Fatal(err)
		}
		return obj
	}
	initial := build([]*music.Info{one, two})
	reordered := build([]*music.Info{two, one})
	if reordered["songCount"] != initial["songCount"] || reordered["duration"] != initial["duration"] || reordered["changed"].(string) <= initial["changed"].(string) {
		t.Fatalf("同数量、同时长的歌曲重排必须更新版本: %+v %+v", initial, reordered)
	}
	one.Raw["name"] = "歌曲改名"
	renamedTrack := build([]*music.Info{two, one})
	if renamedTrack["changed"].(string) <= reordered["changed"].(string) {
		t.Fatal("歌曲元数据更新没有推进版本")
	}
	board.Name = "新榜名"
	renamedBoard := build([]*music.Info{two, one})
	if renamedBoard["changed"].(string) <= renamedTrack["changed"].(string) {
		t.Fatal("榜单改名没有推进版本")
	}
	board.Name = ""
	if got := build(nil); got["changed"] != renamedBoard["changed"] || got["name"] != renamedBoard["name"] || got["songCount"] != 2 {
		t.Fatalf("缓存丢失时不能退回未知名称或一首占位摘要: %+v", got)
	}
}

func TestBoardPlaylistVersionSurvivesCacheLossAndRestart(t *testing.T) {
	f := newDirectoryTestServer(t)
	ctx := context.Background()
	if _, err := f.store.Update(ctx, map[string]json.RawMessage{"boardSources": json.RawMessage(`["wy"]`)}); err != nil {
		t.Fatal(err)
	}
	remote := func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if strings.HasSuffix(path, ".getBoards") {
			return json.RawMessage(`{"list":[{"name":"榜单一","bangid":"one"}]}`), nil
		}
		return json.RawMessage(`{"list":[{"songmid":"one","name":"歌曲一","interval":"00:30"},{"songmid":"two","name":"歌曲二","interval":"00:45"}],"total":2,"limit":100,"page":1}`), nil
	}
	f.server.Catalog.SetRemoteCallerForTest(remote)
	f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists?f=json")
	before := f.boardRequest(t, f.server.getPlaylist, "/rest/getPlaylist?id=lb-wy-one&f=json")["playlist"].(map[string]any)
	restartCatalog := func() {
		f.server.Catalog.StopBoardWarm()
		catalog := music.NewCatalog(f.database, nil, nil, f.store, f.server.Log)
		catalog.SetRemoteCallerForTest(remote)
		t.Cleanup(catalog.StopBoardWarm)
		f.server.Catalog = catalog
	}
	assertSummary := func(got map[string]any) {
		t.Helper()
		for _, key := range []string{"name", "comment", "changed", "songCount", "duration"} {
			if got[key] != before[key] {
				t.Fatalf("重启后 %s 发生无内容变化的漂移: %v %v", key, before[key], got[key])
			}
		}
	}
	restartCatalog()
	// 名称缓存尚未恢复，直接打开详情仍使用保存的名称与同一个内容版本。
	assertSummary(f.boardRequest(t, f.server.getPlaylist, "/rest/getPlaylist?id=lb-wy-one&f=json")["playlist"].(map[string]any))
	f.server.Catalog.StopBoardWarm()
	if err := f.database.ClearBoardSnapshots(ctx); err != nil {
		t.Fatal(err)
	}
	restartCatalog()
	list := f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists?f=json")["playlists"].(map[string]any)["playlist"].([]any)
	assertSummary(list[0].(map[string]any))
	assertSummary(f.boardRequest(t, f.server.getPlaylist, "/rest/getPlaylist?id=lb-wy-one&f=json")["playlist"].(map[string]any))
}

func TestUserPlaylistCanStillBeEmpty(t *testing.T) {
	f := newDirectoryTestServer(t)
	if err := f.database.CreatePlaylist(context.Background(), "pl-empty", f.user.ID, "空歌单", nil); err != nil {
		t.Fatal(err)
	}
	root := f.boardRequest(t, f.server.getPlaylist, "/rest/getPlaylist?id=pl-empty&f=json")
	if root["status"] != "ok" {
		t.Fatalf("正常用户空歌单不能被当作上游故障: %+v", root)
	}
	playlist := root["playlist"].(map[string]any)
	if playlist["songCount"] != float64(0) || len(playlist["entry"].([]any)) != 0 {
		t.Fatalf("用户空歌单内容不正确: %+v", playlist)
	}
}
