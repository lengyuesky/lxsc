package fnmusic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

func TestPlaylistWritesPersistPrivateAndShareExistingDomain(t *testing.T) {
	f := newFixture(t)
	alice, bob := login(t, f, "alice"), login(t, f, "bob")
	_, _ = f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"publicPlaylists": json.RawMessage(`true`)})
	data := payload(t, request(f, http.MethodPost, "/music/api/v1/playlist/create", alice, map[string]any{"name": "  私有歌单  ", "public": true, "userId": f.users["bob"].ID}))
	guid := data["guid"].(string)
	id, err := f.server.resolve(context.Background(), "playlist", guid)
	if err != nil {
		t.Fatal(err)
	}
	playlist, _ := f.db.GetPlaylist(context.Background(), id)
	if playlist.Public || playlist.UserID != f.users["alice"].ID || playlist.Name != "私有歌单" {
		t.Fatal("创建不能沿用全站公开默认或客户端伪造的归属", playlist)
	}
	if request(f, http.MethodGet, "/music/api/v1/playlist/detail?guid="+guid, bob, nil).Code != http.StatusNotFound {
		t.Fatal("新歌单泄漏给其他用户")
	}
	payload(t, request(f, http.MethodPost, "/music/api/v1/playlist/edit", alice, map[string]any{"guid": guid, "name": "已改名"}))
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", alice, nil))
	first, second := identifier("track", "tr-wy-one"), identifier("track", "tr-wy-public")
	for _, ids := range [][]string{{first}, {second, first, second}, {first}} {
		payload(t, request(f, http.MethodPost, "/music/api/v1/playlist/add-track", alice, map[string]any{"guid": guid, "trackGUIDs": ids}))
	}
	playlist, _ = f.db.GetPlaylist(context.Background(), id)
	if playlist.Name != "已改名" || !reflect.DeepEqual(playlist.TrackIDs, []string{"tr-wy-public", "tr-wy-one"}) {
		t.Fatal("重复加歌或顺序与共享语义不同", playlist)
	}
	_ = f.db.ReplacePlaylistTracks(context.Background(), id, []string{"tr-wy-one", "tr-wy-public", "tr-wy-one"})
	payload(t, request(f, http.MethodPost, "/music/api/v1/playlist/remove-track", alice, map[string]any{"guid": guid, "trackGUIDs": []string{first}}))
	playlist, _ = f.db.GetPlaylist(context.Background(), id)
	if !reflect.DeepEqual(playlist.TrackIDs, []string{"tr-wy-public"}) {
		t.Fatal("移除必须删除同ID的全部位置", playlist.TrackIDs)
	}
	alice = login(t, f, "alice")
	if payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/detail?guid="+guid, alice, nil))["name"] != "已改名" {
		t.Fatal("重新登录没有读取持久状态")
	}
	payload(t, request(f, http.MethodPost, "/music/api/v1/playlist/delete", alice, map[string]any{"guid": guid}))
	if request(f, http.MethodGet, "/music/api/v1/playlist/detail?guid="+guid, alice, nil).Code != http.StatusNotFound {
		t.Fatal("已删除歌单仍可读取")
	}
	seen := map[string]bool{}
	for _, event := range f.server.Diagnostics.Recent(2000) {
		if event.Method == http.MethodPost {
			seen[event.Endpoint] = true
		}
	}
	for _, endpoint := range []string{"createPlaylist", "updatePlaylist", "deletePlaylist"} {
		if !seen[endpoint] {
			t.Fatal("写接口诊断丢失操作名称", endpoint)
		}
	}
}

func TestSearchWritesBecomeDurableWithoutSavingUnselectedResults(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	alice, other, bob := login(t, f, "alice"), login(t, f, "alice"), login(t, f, "bob")
	search := searchRequest(t, f, alice, "track", "remote", "")
	guid := search["list"].([]any)[0].(map[string]any)["guid"].(string)
	for _, token := range []string{other, bob} {
		if request(f, http.MethodPost, "/music/api/v1/favorite-track/create", token, map[string]any{"trackGUID": guid}).Code != http.StatusNotFound {
			t.Fatal("另一会话用临时GUID创建持久收藏")
		}
	}
	payload(t, request(f, http.MethodPost, "/music/api/v1/favorite-track/create", alice, map[string]any{"trackGUID": guid}))
	payload(t, request(f, http.MethodPost, "/music/api/v1/favorite-track/create", alice, map[string]any{"trackGUID": guid}))
	stars, _ := f.db.ListStarred(context.Background(), f.users["alice"].ID, "track")
	if len(stars) != 1 || stars[0].ItemID != "tr-wy-remote-one" {
		t.Fatal("重复收藏没有幂等", stars)
	}
	if _, err := f.db.GetTrack(context.Background(), "tr-wy-remote-two"); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("收藏保存了未选中的搜索结果")
	}
	// 更换搜索授权仓及目录缓存，模拟重启后的持久资源读取。
	f.server.searches = searchStore{}
	f.server.Catalog = music.NewCatalog(f.db, nil, nil, f.server.Settings, f.server.Catalog.Log)
	t.Cleanup(f.server.Catalog.StopBoardWarm)
	metadata := payload(t, request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, other, nil))
	if metadata["track"].(map[string]any)["isFavorite"] != true {
		t.Fatal("保存的搜索歌曲没有持久收藏状态")
	}
	for range 2 {
		payload(t, request(f, http.MethodPost, "/music/api/v1/favorite-track/delete", other, map[string]any{"trackGUID": guid}))
	}
	if request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, other, nil).Code != http.StatusNotFound {
		t.Fatal("取消最后一个引用后GUID仍授予权限")
	}
	if len(payload(t, request(f, http.MethodGet, "/music/api/v1/favorite-track/list", other, nil))["list"].([]any)) != 0 {
		t.Fatal("收藏列表没有更新")
	}
}

func TestBatchWriteFailuresLeaveNoMetadataOrMappings(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	alice := login(t, f, "alice")
	search := searchRequest(t, f, alice, "track", "remote", "")
	remote := search["list"].([]any)[0].(map[string]any)["guid"].(string)
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/list", alice, nil))
	pl := identifier("playlist", "pl-alice")
	for _, guids := range [][]string{{remote, strings.Repeat("0", 32)}, {remote, identifier("track", "tr-wy-two")}} {
		w := request(f, http.MethodPost, "/music/api/v1/playlist/add-track", alice, map[string]any{"guid": pl, "trackGUIDs": guids})
		if w.Code != http.StatusNotFound {
			t.Fatal("无效批量操作没有整体失败", w.Code)
		}
	}
	for _, check := range []func() error{
		func() error { _, err := f.db.GetTrack(context.Background(), "tr-wy-remote-one"); return err },
		func() error { _, _, err := f.db.ResolveClientID(context.Background(), "fnmusic", remote); return err },
	} {
		if !errors.Is(check(), db.ErrNotFound) {
			t.Fatal("失败写入留下持久元数据或GUID")
		}
	}
	public := identifier("playlist", "pl-public")
	if request(f, http.MethodPost, "/music/api/v1/playlist/add-track", alice, map[string]any{"guid": public, "trackGUIDs": []string{remote}}).Code != http.StatusForbidden {
		t.Fatal("公开歌单可读被当成可写")
	}
	for _, name := range []string{" ", strings.Repeat("字", 101)} {
		if request(f, http.MethodPost, "/music/api/v1/playlist/create", alice, map[string]any{"name": name}).Code != http.StatusBadRequest {
			t.Fatal("歌单名称未校验")
		}
	}
}

func TestPlaylistAliasCanBeViewedSavedAndRemoved(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	alias := "tr-kg-legacy-id"
	row := db.Track{ID: alias, Source: "kg", Name: "别名歌曲", JSON: []byte(`{"source":"kg","songmid":"123","hash":"ABCDEF","name":"别名歌曲"}`)}
	if err := f.db.UpsertTracks(ctx, []db.Track{row}); err != nil {
		t.Fatal(err)
	}
	_ = f.db.ReplacePlaylistTracks(ctx, "pl-alice", []string{alias, alias})
	alice := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/list", alice, nil))
	pl := identifier("playlist", "pl-alice")
	data := payload(t, request(f, http.MethodGet, "/music/api/v1/track/playlist-detail/list?playlistGUID="+pl, alice, nil))
	guid := data["list"].([]any)[0].(map[string]any)["guid"].(string)
	if guid != identifier("track", alias) {
		t.Fatal("歌单成员别名被规范ID替换")
	}
	payload(t, request(f, http.MethodPost, "/music/api/v1/favorite-track/create", alice, map[string]any{"trackGUID": guid}))
	payload(t, request(f, http.MethodPost, "/music/api/v1/playlist/remove-track", alice, map[string]any{"guid": pl, "trackGUIDs": []string{guid}}))
	playlist, _ := f.db.GetPlaylist(ctx, "pl-alice")
	if playlist.Count != 0 {
		t.Fatal("按别名GUID移除没有生效")
	}
	data = payload(t, request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, alice, nil))
	if data["track"].(map[string]any)["guid"] != guid {
		t.Fatal("保存别名收藏后metadata改变GUID")
	}
}

func uploadCover(t *testing.T, f *fixture, token string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, _ := form.CreateFormFile("file", "test.png")
	_, _ = file.Write(data)
	_ = form.Close()
	r := httptest.NewRequest(http.MethodPost, "/music/api/v1/static/cover/playlist", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Authorization", token)
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	return w
}

func TestUploadedCoverIsValidatedOwnedAndPersisted(t *testing.T) {
	f := newFixture(t)
	alice, bob := login(t, f, "alice"), login(t, f, "bob")
	picture := coverPNG(t)
	cover := payload(t, uploadCover(t, f, alice, picture))["coverId"].(string)
	if request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+cover, bob, nil).Code != http.StatusNotFound {
		t.Fatal("未绑定的上传封面泄漏")
	}
	if request(f, http.MethodPost, "/music/api/v1/playlist/create", bob, map[string]any{"name": "盗用封面", "coverId": cover}).Code != http.StatusForbidden {
		t.Fatal("另一用户可以绑定封面")
	}
	guid := payload(t, request(f, http.MethodPost, "/music/api/v1/playlist/create", alice, map[string]any{"name": "带封面", "coverId": cover}))["guid"].(string)
	view := payload(t, request(f, http.MethodGet, "/music/api/v1/playlist/detail?guid="+guid, alice, nil))
	if view["coverId"] != cover {
		t.Fatal("新封面未保存")
	}
	w := request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+cover, alice, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) {
		t.Fatal("上传封面没有实际回读")
	}
	for _, content := range [][]byte{[]byte("<svg>bad</svg>"), []byte(`{"error":true}`), picture[:20], append([]byte("RIFF0000WEBPVP8 "), make([]byte, 8)...)} {
		if uploadCover(t, f, alice, content).Code < 400 {
			t.Fatal("伪造或不完整图片被接受")
		}
	}
}

func TestWriteAuthorityRechecksSearchExpiry(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	alice := login(t, f, "alice")
	result := searchRequest(t, f, alice, "track", "remote", "")
	guid := result["list"].([]any)[0].(map[string]any)["guid"].(string)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", alice)
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), userKey{}, f.users["alice"]), sessionKey{}, sha256.Sum256([]byte(alice))))
	tracks, mappings, authority, err := f.server.writeTracksFor(r, []string{guid})
	if err != nil {
		t.Fatal(err)
	}
	f.server.searches.mu.Lock()
	for _, key := range f.server.searches.entries.Keys() {
		entry, _ := f.server.searches.entries.Peek(key)
		entry.expires = time.Now().Add(-time.Minute)
		f.server.searches.entries.Add(key, entry)
	}
	f.server.searches.mu.Unlock()
	if err := f.db.StarTracksAuthorized(r.Context(), f.users["alice"].ID, tracks, authority, mappings...); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("进入事务后没有复查临时授权过期", err)
	}
	if _, err := f.db.GetTrack(r.Context(), tracks[0].ID); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("失效授权写入了元数据")
	}
}
