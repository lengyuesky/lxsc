package db

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlaylistManagement(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	u1, err := database.CreateUser(ctx, "alice", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := database.CreateUser(ctx, "bob", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}

	if err := database.CreatePlaylistFull(ctx, "pl-one", u1.ID, "收藏", "备注", true, []string{"tr-wy-1", "tr-tx-2"}); err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePlaylistFull(ctx, "pl-two", u2.ID, "私有", "", false, nil); err != nil {
		t.Fatal(err)
	}

	all, err := database.ListAllPlaylists(ctx, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("全部歌单结果异常: len=%d err=%v", len(all), err)
	}
	owned, err := database.ListAllPlaylists(ctx, u1.ID)
	if err != nil || len(owned) != 1 || owned[0].ID != "pl-one" || owned[0].Count != 2 {
		t.Fatalf("按用户筛选异常: %+v err=%v", owned, err)
	}
	visible, err := database.ListPlaylists(ctx, u2.ID)
	if err != nil || len(visible) != 2 {
		t.Fatalf("可见歌单异常: len=%d err=%v", len(visible), err)
	}
	if visible[0].ID != "pl-two" || visible[1].ID != "pl-one" {
		t.Fatalf("当前用户自建歌单应排在其他公开歌单前面: %+v", visible)
	}

	name, comment, public := "新名称", "新备注", false
	if err := database.UpdatePlaylistMeta(ctx, "pl-one", &name, &comment, &public); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"tr-tx-2", "tr-wy-1", "tr-tx-2"}
	if err := database.ReplacePlaylistTracks(ctx, "pl-one", wantOrder); err != nil {
		t.Fatal(err)
	}
	playlist, err := database.GetPlaylist(ctx, "pl-one")
	if err != nil {
		t.Fatal(err)
	}
	if playlist.Name != name || playlist.Comment != comment || playlist.Public || !reflect.DeepEqual(playlist.TrackIDs, wantOrder) {
		t.Fatalf("歌单更新异常: %+v", playlist)
	}

	if err := database.ReplacePlaylistTracks(ctx, "pl-missing", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("替换不存在歌单应返回 ErrNotFound，实际为 %v", err)
	}
	if err := database.DeletePlaylist(ctx, "pl-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetPlaylist(ctx, "pl-one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("歌单应已删除，实际为 %v", err)
	}
	if err := database.DeletePlaylist(ctx, "pl-one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应返回 ErrNotFound，实际为 %v", err)
	}
}
