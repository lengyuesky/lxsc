package portal

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

func responseTrackIDs(out map[string]any) []string {
	ids := []string{}
	for _, value := range out["tracks"].([]any) {
		ids = append(ids, value.(map[string]any)["id"].(string))
	}
	return ids
}

func cacheTemporaryTrack(f *portalFixture, id string) string {
	in := music.FromMap(map[string]any{"source": "wy", "songmid": id, "name": "临时歌曲", "singer": "歌手", "albumName": "专辑", "interval": "03:45"})
	f.service.Catalog.Cache([]*music.Info{in})
	return in.TrackID()
}

func TestCreateCollectAndReplacePersistTemporaryMetadata(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.client(t, "alice")
	ctx := context.Background()
	trackID := cacheTemporaryTrack(f, "temporary-create")
	if _, err := f.db.GetTrack(ctx, trackID); err == nil {
		t.Fatal("浏览临时歌曲不得提前持久化")
	}
	status, created := doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists", map[string]any{"name": "即时收藏", "trackIds": []string{trackID, trackID}})
	if status != 200 || created["songCount"] != float64(1) || created["userId"] != float64(f.users["alice"].ID) {
		t.Fatalf("新建并收藏失败: %d %+v", status, created)
	}
	if created["tracks"].([]any)[0].(map[string]any)["duration"] != float64(225) {
		t.Fatal("歌曲视图应包含时长秒数")
	}
	playlistID := created["id"].(string)
	revision := created["tracksRevision"].(string)
	secondID := cacheTemporaryTrack(f, "temporary-add")
	status, added := doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists/"+playlistID+"/tracks", map[string]any{"trackId": secondID})
	if status != 200 || added["added"] != true {
		t.Fatalf("增量收藏失败: %d %+v", status, added)
	}
	detail := added["playlist"].(map[string]any)
	if !reflect.DeepEqual(responseTrackIDs(detail), []string{secondID, trackID}) || revision == detail["tracksRevision"] {
		t.Fatalf("置顶或版本更新错误: %+v", detail)
	}
	status, duplicate := doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists/"+playlistID+"/tracks", map[string]any{"trackId": trackID})
	if status != 200 || duplicate["added"] != false || !reflect.DeepEqual(responseTrackIDs(duplicate["playlist"].(map[string]any)), []string{secondID, trackID}) {
		t.Fatalf("重复收藏应原样返回: %d %+v", status, duplicate)
	}
	status, _ = doJSON(t, alice, http.MethodPut, f.server.URL+"/playlists/"+playlistID+"/tracks", map[string]any{"trackIds": []string{trackID}, "expectedRevision": revision})
	if status != http.StatusConflict {
		t.Fatalf("旧草稿应冲突: %d", status)
	}
	thirdID := cacheTemporaryTrack(f, "temporary-replace")
	want := []string{thirdID, thirdID, secondID}
	status, replaced := doJSON(t, alice, http.MethodPut, f.server.URL+"/playlists/"+playlistID+"/tracks", map[string]any{"trackIds": want, "expectedRevision": detail["tracksRevision"]})
	if status != 200 || !reflect.DeepEqual(responseTrackIDs(replaced), want) {
		t.Fatalf("网页保存应持久化且保留重复: %d %+v", status, replaced)
	}
	// 重建目录对象模拟服务重启，不能依赖之前的搜索缓存恢复信息。
	f.service.Catalog = music.NewCatalog(f.db, nil, nil, f.service.Settings, f.service.Log)
	status, reloaded := doJSON(t, alice, http.MethodGet, f.server.URL+"/playlists/"+playlistID, nil)
	if status != 200 || !reflect.DeepEqual(responseTrackIDs(reloaded), want) || reloaded["tracksRevision"] != replaced["tracksRevision"] {
		t.Fatalf("重启后歌单应稳定: %d %+v", status, reloaded)
	}
	for _, value := range reloaded["tracks"].([]any) {
		track := value.(map[string]any)
		if track["unavailable"] == true || track["name"] != "临时歌曲" {
			t.Fatalf("保存后元数据不可丢失: %+v", track)
		}
	}
	for _, id := range []string{trackID, secondID, thirdID} {
		if _, err := f.db.GetTrack(ctx, id); err != nil {
			t.Fatalf("未保存元数据 %s: %v", id, err)
		}
	}
}

func TestCollectPermissionsValidationAndCapacity(t *testing.T) {
	f := newPortalFixture(t)
	alice, bob, admin := f.client(t, "alice"), f.client(t, "bob"), f.client(t, "admin")
	base := f.server.URL + "/playlists"
	_, created := doJSON(t, alice, http.MethodPost, base, map[string]any{"name": "公开", "public": true})
	playlistID := created["id"].(string)
	addURL := base + "/" + playlistID + "/tracks"
	for _, tc := range []struct {
		name   string
		client *http.Client
		url    string
		body   any
		status int
	}{
		{"未登录", http.DefaultClient, addURL, map[string]any{"trackId": "tr-wy-1"}, 401},
		{"他人公开歌单不可改", bob, addURL, map[string]any{"trackId": "tr-wy-1"}, 403},
		{"歌单已删除", alice, base + "/pl-missing/tracks", map[string]any{"trackId": "tr-wy-1"}, 404},
		{"无效 ID", alice, addURL, map[string]any{"trackId": "bad"}, 400},
		{"非歌曲 ID", alice, addURL, map[string]any{"trackId": "al-wy-1"}, 400},
		{"未知平台", alice, addURL, map[string]any{"trackId": "tr-unknown-1"}, 400},
		{"缺少平台内 ID", alice, addURL, map[string]any{"trackId": "tr-wy"}, 400},
		{"歌曲不存在", alice, addURL, map[string]any{"trackId": "tr-wy-missing"}, 400},
		{"管理员代加", admin, addURL, map[string]any{"trackId": "tr-wy-1"}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out := doJSON(t, tc.client, http.MethodPost, tc.url, tc.body)
			if status != tc.status {
				t.Fatalf("状态错误: %d %+v", status, out)
			}
		})
	}
	ids := make([]string, maxPlaylistTracks)
	for i := range ids {
		ids[i] = fmt.Sprintf("tr-wy-%d", i+1)
	}
	if err := f.db.ReplacePlaylistTracks(context.Background(), playlistID, ids); err != nil {
		t.Fatal(err)
	}
	status, out := doJSON(t, alice, http.MethodPost, addURL, map[string]any{"trackId": "tr-tx-2"})
	if status != 400 {
		t.Fatalf("满歌单不能新增: %d %+v", status, out)
	}
	// 容量检查在解析前完成，避免超限新建残留空歌单。
	tooMany := append(ids, "tr-wy-extra")
	before, _ := f.db.ListAllPlaylists(context.Background(), 0)
	status, _ = doJSON(t, alice, http.MethodPost, base, map[string]any{"name": "过多", "trackIds": tooMany})
	after, _ := f.db.ListAllPlaylists(context.Background(), 0)
	if status != 400 || len(before) != len(after) {
		t.Fatal("超限新建不能留下空歌单")
	}
	status, _ = doJSON(t, bob, http.MethodPost, base, map[string]any{"name": "越权", "ownerId": f.users["alice"].ID, "trackIds": []string{"tr-wy-1"}})
	if status != 403 {
		t.Fatal("新建并收藏不能绕过归属权限")
	}
}

func TestPlaylistPreservesAliasMetadataAndLegacyReplacement(t *testing.T) {
	f := newPortalFixture(t)
	ctx := context.Background()
	alias := "tr-kg-legacy-id"
	row := db.Track{ID: alias, Source: "kg", Name: "别名歌曲", JSON: []byte(`{"source":"kg","songmid":"123","hash":"ABCDEF","name":"别名歌曲"}`)}
	if err := f.db.UpsertTracks(ctx, []db.Track{row}); err != nil {
		t.Fatal(err)
	}
	alice := f.client(t, "alice")
	_, created := doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists", map[string]any{"name": "别名"})
	url := f.server.URL + "/playlists/" + created["id"].(string)
	status, updated := doJSON(t, alice, http.MethodPut, url+"/tracks", map[string]any{"trackIds": []string{alias, alias}})
	if status != 200 || !reflect.DeepEqual(responseTrackIDs(updated), []string{alias, alias}) {
		t.Fatalf("旧无版本替换契约改变: %d %+v", status, updated)
	}
	f.service.Catalog = music.NewCatalog(f.db, nil, nil, f.service.Settings, f.service.Log)
	_, updated = doJSON(t, alice, http.MethodGet, url, nil)
	if updated["tracks"].([]any)[0].(map[string]any)["name"] != "别名歌曲" || responseTrackIDs(updated)[0] != alias {
		t.Fatalf("不能把歌单别名改为上游返回的规范 ID: %+v", updated)
	}
}
