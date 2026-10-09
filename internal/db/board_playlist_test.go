package db

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestBoardPlaylistMetadataVersionsAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if database != nil {
			database.Close()
		}
	}()
	syncMeta := func(name, comment string, content *BoardPlaylistContent) BoardPlaylistMetadata {
		t.Helper()
		meta, err := database.SyncBoardPlaylistMetadata(ctx, "lb-wy-one", name, comment, content)
		if err != nil {
			t.Fatal(err)
		}
		return meta
	}
	initial := syncMeta("榜单", "旧简介", nil)
	if initial.Count != 1 || initial.UpdatedAt <= 0 {
		t.Fatalf("未加载榜单必须有可打开入口和持久版本: %+v", initial)
	}
	if got := syncMeta("榜单", "旧简介", nil); got != initial {
		t.Fatalf("重复读取不能改动版本: %+v %+v", initial, got)
	}
	content := &BoardPlaylistContent{Count: 2, Duration: 90, Hash: "content-one"}
	loaded := syncMeta("榜单", "旧简介", content)
	if loaded.Count != 2 || loaded.Duration != 90 || loaded.UpdatedAt <= initial.UpdatedAt {
		t.Fatalf("首次完整加载必须更新摘要和版本: %+v", loaded)
	}
	renamed := syncMeta("新榜名", "旧简介", content)
	changed := syncMeta("新榜名", "在线榜单，只读", content)
	if renamed.UpdatedAt <= loaded.UpdatedAt || changed.UpdatedAt <= renamed.UpdatedAt {
		t.Fatal("改名和修改简介必须各自更新版本")
	}
	if got := syncMeta("", "在线榜单，只读", nil); got != changed {
		t.Fatalf("名称或歌曲缓存丢失不能覆盖已知摘要: %+v", got)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := syncMeta("", "在线榜单，只读", content); got != changed {
		t.Fatalf("重启不能改名、归零或推进版本: %+v %+v", changed, got)
	}
	content.Hash = "same-count-new-tracks"
	if got := syncMeta("", "在线榜单，只读", content); got.Count != changed.Count || got.UpdatedAt <= changed.UpdatedAt {
		t.Fatalf("同数量换歌也必须推进版本: %+v", got)
	}
}

func TestBoardPlaylistMetadataConcurrentVersion(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	before, err := database.SyncBoardPlaylistMetadata(ctx, "lb-wy-one", "榜单", "简介", nil)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 16
	versions := make(chan int64, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			meta, err := database.SyncBoardPlaylistMetadata(ctx, "lb-wy-one", "榜单", "简介", &BoardPlaylistContent{Count: 2, Duration: 60, Hash: "complete"})
			if err != nil {
				t.Error(err)
				return
			}
			versions <- meta.UpdatedAt
		})
	}
	wg.Wait()
	close(versions)
	var first int64
	for version := range versions {
		if first == 0 {
			first = version
		}
		if version != first || version <= before.UpdatedAt {
			t.Fatalf("并发读取同一内容必须共享版本: %d %d", first, version)
		}
	}
}

func TestBoardPlaylistMetadataMigrationFromV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, err := database.CreateUser(ctx, "test", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePlaylist(ctx, "pl-existing", user.ID, "已有歌单", []string{"tr-wy-one"}); err != nil {
		t.Fatal(err)
	}
	// 模拟升级前的数据库结构，已有用户和歌单必须继续可用。
	for _, statement := range []string{"DROP TABLE board_playlist_metadata", "DELETE FROM schema_migrations WHERE version=5"} {
		if _, err := database.sql.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	playlist, err := database.GetPlaylist(ctx, "pl-existing")
	if err != nil || playlist.Name != "已有歌单" || len(playlist.TrackIDs) != 1 {
		t.Fatalf("升级丢失已有歌单: %+v %v", playlist, err)
	}
	if _, err := database.SyncBoardPlaylistMetadata(ctx, "lb-wy-one", "榜单", "简介", nil); err != nil {
		t.Fatal(err)
	}
}
