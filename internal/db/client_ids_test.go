package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestClientIDMigrationPreservesExistingMusicData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, err := database.CreateUser(ctx, "existing-user", "encrypted-original", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePlaylist(ctx, "existing-playlist", user.ID, "原有歌单", []string{"tr-wy-existing"}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertTracks(ctx, []Track{{ID: "tr-wy-existing", Source: "wy", Name: "原有曲目", JSON: []byte(`{"source":"wy","songmid":"existing"}`)}}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"DROP TABLE client_ids", "DELETE FROM schema_migrations WHERE version=6"} {
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
	got, err := database.GetUserByID(ctx, user.ID)
	if err != nil || got.PasswordEnc != "encrypted-original" {
		t.Fatal("迁移改变了已有密码")
	}
	playlist, err := database.GetPlaylist(ctx, "existing-playlist")
	if err != nil || playlist.Name != "原有歌单" || len(playlist.TrackIDs) != 1 {
		t.Fatal("迁移改变了已有歌单")
	}
	if _, err := database.GetTrack(ctx, "tr-wy-existing"); err != nil {
		t.Fatal("迁移丢失元数据")
	}
	if err := database.EnsureClientID(ctx, "fnmusic", "track", "tr-wy-existing", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	kind, id, err := database.ResolveClientID(ctx, "fnmusic", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || kind != "track" || id != "tr-wy-existing" {
		t.Fatal("映射表未迁移")
	}
	if err := database.EnsureClientID(ctx, "fnmusic", "track", "tr-wy-other", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("映射冲突未拒绝")
	}
	if _, _, err := database.ResolveClientID(ctx, "other-protocol", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatal("协议标识交叉泄漏")
	}
}
