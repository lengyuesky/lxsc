package subsonic

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// TestGetPlaylistsWarmsVisibleBoards 校验歌单列表返回后会预热尚无快照的可见榜单。
func TestGetPlaylistsWarmsVisibleBoards(t *testing.T) {
	f := newDirectoryTestServer(t)
	f.server.Catalog.EnableBoardWarm()
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"boardSources": json.RawMessage(`["wy"]`)}); err != nil {
		t.Fatal(err)
	}
	var loads atomic.Int32
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path == "wy.leaderboard.getBoards" {
			return json.RawMessage(`{"list":[{"name":"榜单一","bangid":"one"},{"name":"榜单二","bangid":"two"}]}`), nil
		}
		loads.Add(1)
		return json.RawMessage(`{"list":[{"songmid":"1","name":"歌曲","interval":"00:30"}],"total":1,"limit":100,"page":1}`), nil
	})
	root := f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists?f=json&c=Amcfy")
	list, _ := root["playlists"].(map[string]any)["playlist"].([]any)
	if len(list) != 2 {
		t.Fatalf("歌单列表异常: %v", root)
	}
	deadline := time.After(3 * time.Second)
	for _, boardID := range []string{"one", "two"} {
		for {
			if count, _, ok := f.server.Catalog.CachedBoardSummary("wy", boardID); ok && count == 1 {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("可见榜单 %s 未被预热", boardID)
			case <-time.After(time.Millisecond):
			}
		}
	}
	if loads.Load() != 2 {
		t.Fatalf("预热请求次数异常: %d", loads.Load())
	}
}

// TestStartupWarmupScansVisibleBoards 校验启动扫描会读取可见榜单目录并预热。
func TestStartupWarmupScansVisibleBoards(t *testing.T) {
	f := newDirectoryTestServer(t)
	f.server.Catalog.EnableBoardWarm()
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"boardSources": json.RawMessage(`["wy"]`)}); err != nil {
		t.Fatal(err)
	}
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if path == "wy.leaderboard.getBoards" {
			return json.RawMessage(`{"list":[{"name":"榜单一","bangid":"one"}]}`), nil
		}
		return json.RawMessage(`{"list":[{"songmid":"1","name":"歌曲","interval":"00:30"}],"total":1,"limit":100,"page":1}`), nil
	})
	f.server.warmVisibleBoardsOnce(context.Background())
	deadline := time.After(3 * time.Second)
	for {
		if count, _, ok := f.server.Catalog.CachedBoardSummary("wy", "one"); ok && count == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("启动扫描未预热榜单")
		case <-time.After(time.Millisecond):
		}
	}
}
