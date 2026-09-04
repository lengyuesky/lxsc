package settings

import (
	"context"
	"path/filepath"
	"testing"

	"lxsc/internal/db"
)

func TestLegacyBoardsSettingKeepsCurrentValue(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.SetSetting(ctx, "showBoards", "true"); err != nil {
		t.Fatal(err)
	}
	store, err := New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Get().ShowBoards {
		t.Fatal("升级后应保留已开启的榜单目录设置")
	}
	if got := database.GetSetting(ctx, "showBoards", ""); got != "true" {
		t.Fatalf("设置值不应被升级流程改写: %q", got)
	}
}
