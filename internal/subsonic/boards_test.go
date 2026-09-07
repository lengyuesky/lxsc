package subsonic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"lxsc/internal/music"
)

func (fixture directoryTestServer) boardRequest(t *testing.T, handler http.HandlerFunc, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(withUser(req.Context(), fixture.user))
	rec := httptest.NewRecorder()
	handler(rec, req)
	var body map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body["subsonic-response"]
}

func objectIDs(raw any) []string {
	ids := []string{}
	items, _ := raw.([]any)
	for _, item := range items {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	return ids
}

func TestBoardSelectionsMatchAcrossDirectoriesAndPlaylists(t *testing.T) {
	f := newDirectoryTestServer(t)
	ctx := context.Background()
	if err := f.database.CreatePlaylist(ctx, "pl-user", f.user.ID, "我的歌单", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Update(ctx, map[string]json.RawMessage{
		"boardSources":    json.RawMessage(`["tx","wy","kw","tx"]`),
		"boardSelections": json.RawMessage(`{"tx":["same"],"wy":["two"],"kw":[],"kg":["same"]}`),
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	paths := []string{}
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		if path != "tx.leaderboard.getBoards" && path != "wy.leaderboard.getBoards" {
			return nil, fmt.Errorf("不应加载未展示的平台或歌曲: %s", path)
		}
		return json.RawMessage(`{"list":[{"name":"同名榜单","bangid":"same"},{"id":"另一个标识","name":"第二榜单","bangid":"two"},{"name":"新增榜单","bangid":"new"}]}`), nil
	})
	before, _ := f.database.Statistics(ctx)
	root := f.directory(t, "1")
	if got := objectIDs(root["child"]); !reflect.DeepEqual(got, []string{music.ArtistCategoryID("root"), "dir-tx", "dir-wy"}) {
		t.Fatalf("选空平台必须隐藏，平台顺序和去重应保留: %v", got)
	}
	if len(paths) != 0 {
		t.Fatal("根目录不能预加载榜单")
	}
	for _, item := range []struct{ source, id string }{{"tx", "same"}, {"wy", "two"}} {
		got := objectIDs(f.directory(t, "dir-"+item.source)["child"])
		if !reflect.DeepEqual(got, []string{music.BoardID(item.source, item.id)}) {
			t.Fatalf("目录必须按平台和 bangid 筛选: %s %v", item.source, got)
		}
	}
	response := f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists.view?f=json")
	want := []string{"pl-user", music.BoardID("tx", "same"), music.BoardID("wy", "two")}
	if got := objectIDs(response["playlists"].(map[string]any)["playlist"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("原生歌单展示必须与目录一致且保留用户歌单: %v", got)
	}
	// 仅修改选择就应立即改变结果，无需失效已有名称缓存。
	if _, err := f.store.Update(ctx, map[string]json.RawMessage{"boardSelections": json.RawMessage(`{"wy":["same"],"tx":[],"kw":[]}`)}); err != nil {
		t.Fatal(err)
	}
	response = f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists.view?f=json")
	if got := objectIDs(response["playlists"].(map[string]any)["playlist"]); !reflect.DeepEqual(got, []string{"pl-user", music.BoardID("wy", "same")}) {
		t.Fatalf("更改选择后必须立即筛选缓存结果: %v", got)
	}
	if got := objectIDs(f.directory(t, "dir-wy")["child"]); !reflect.DeepEqual(got, []string{music.BoardID("wy", "same")}) {
		t.Fatalf("目录未立即更新: %v", got)
	}
	response = f.boardRequest(t, f.server.getMusicDirectory, "/rest/getMusicDirectory.view?id=dir-tx&f=json")
	if response["status"] != "failed" {
		t.Fatal("已隐藏平台不应继续列出榜单")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("更改选择不应清除名称缓存或扫描歌曲: %v", paths)
	}
	after, _ := f.database.Statistics(ctx)
	if before.Tracks != after.Tracks || before.Albums != after.Albums || before.Artists != after.Artists {
		t.Fatalf("榜单列表不能持久化歌曲元数据: %+v %+v", before, after)
	}
}

func TestBoardSelectionSurvivesMissingAndRenamedBoard(t *testing.T) {
	f := newDirectoryTestServer(t)
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{
		"boardSources":    json.RawMessage(`["wy"]`),
		"boardSelections": json.RawMessage(`{"wy":["one"]}`),
	}); err != nil {
		t.Fatal(err)
	}
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path != "wy.leaderboard.getBoards" {
			return nil, fmt.Errorf("不应请求歌曲: %s", path)
		}
		return json.RawMessage(`{"list":[{"name":"新增榜单","bangid":"new"}]}`), nil
	})
	if ids := objectIDs(f.directory(t, "dir-wy")["child"]); len(ids) != 0 {
		t.Fatalf("自定义模式不能自动加入新增榜单: %v", ids)
	}
	for _, label := range []string{"原名称", "修改后的名称"} {
		// 重建目录缓存模拟服务重启，保存的选择仍使用同一份设置。
		catalog := music.NewCatalog(f.database, nil, nil, f.store, f.server.Log)
		catalog.SetRemoteCallerForTest(func(_ context.Context, _ string, _ ...any) (json.RawMessage, error) {
			return json.Marshal(map[string]any{"list": []map[string]any{{"name": label, "bangid": "one"}}})
		})
		f.server.Catalog = catalog
		dir := f.directory(t, "dir-wy")
		items := dir["child"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["name"] != label || items[0].(map[string]any)["id"] != music.BoardID("wy", "one") {
			t.Fatalf("暂时缺失和改名不能丢失选择: %+v", dir)
		}
	}
	if !reflect.DeepEqual(f.store.Get().BoardSelections, map[string][]string{"wy": {"one"}}) {
		t.Fatal("目录请求不能改写已保存选择")
	}
}

func TestHiddenBoardsKeepDirectReadOnlyAccess(t *testing.T) {
	f := newDirectoryTestServer(t)
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"showBoards": json.RawMessage(`false`), "boardSources": json.RawMessage(`[]`), "boardSelections": json.RawMessage(`{"wy":[]}`)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		calls++
		if path != "wy.leaderboard.getList" {
			return nil, fmt.Errorf("深链不得附带目录扫描: %s", path)
		}
		return json.RawMessage(`{"list":[{"source":"wy","songmid":"one","name":"歌曲","singer":"歌手"}]}`), nil
	})
	if ids := objectIDs(f.directory(t, "1")["child"]); len(ids) != 1 {
		t.Fatalf("总开关关闭后应只保留歌手入口: %v", ids)
	}
	list := f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists.view?f=json")
	if len(objectIDs(list["playlists"].(map[string]any)["playlist"])) != 0 || calls != 0 {
		t.Fatal("关闭展示不得读取榜单")
	}
	id := music.BoardID("wy", "one")
	if children := f.directory(t, id)["child"].([]any); len(children) != 1 {
		t.Fatal("原有目录深链应继续读取歌曲")
	}
	response := f.boardRequest(t, f.server.getPlaylist, "/rest/getPlaylist.view?id="+id+"&f=json")
	if response["status"] != "ok" || calls != 1 {
		t.Fatalf("原有歌单深链应继续读取并复用歌曲缓存: %+v %d", response, calls)
	}
}

func TestBoardPlaylistUsesOneSettingsSnapshot(t *testing.T) {
	f := newDirectoryTestServer(t)
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"boardSources": json.RawMessage(`["wy"]`), "boardSelections": json.RawMessage(`{"wy":["one"]}`)}); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	f.server.Catalog.SetRemoteCallerForTest(func(ctx context.Context, path string, _ ...any) (json.RawMessage, error) {
		if !strings.HasSuffix(path, ".getBoards") {
			return nil, fmt.Errorf("意外调用: %s", path)
		}
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return json.RawMessage(`{"list":[{"name":"一","bangid":"one"},{"name":"二","bangid":"two"}]}`), nil
	})
	req := httptest.NewRequest(http.MethodGet, "/rest/getPlaylists.view?f=json", nil)
	req = req.WithContext(withUser(req.Context(), f.user))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.server.getPlaylists(rec, req) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("榜单请求未开始")
	}
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"boardSelections": json.RawMessage(`{"wy":["two"]}`)}); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("榜单请求未完成")
	}
	var result map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	ids := objectIDs(result["subsonic-response"]["playlists"].(map[string]any)["playlist"])
	if !reflect.DeepEqual(ids, []string{music.BoardID("wy", "one")}) {
		t.Fatalf("进行中的请求应使用开始时的设置快照: %v", ids)
	}
	response := f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists.view?f=json")
	if ids := objectIDs(response["playlists"].(map[string]any)["playlist"]); !reflect.DeepEqual(ids, []string{music.BoardID("wy", "two")}) {
		t.Fatalf("后续请求应使用新设置: %v", ids)
	}
}
