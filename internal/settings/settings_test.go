package settings

import (
	"context"
	"encoding/json"
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

func TestStreamModesPersistAndReload(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if store.Get().StreamMode != "redirect" {
		t.Fatal("默认播放方式应保持普通 302")
	}
	for _, mode := range []string{"force_redirect", "proxy", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := json.Marshal(mode)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := store.Update(ctx, map[string]json.RawMessage{"streamMode": raw})
			if err != nil {
				t.Fatal(err)
			}
			if updated.StreamMode != mode || store.Get().StreamMode != mode || database.GetSetting(ctx, "streamMode", "") != mode {
				t.Fatalf("播放方式未同步保存到响应、内存和数据库: %+v", updated)
			}
			reloaded, err := New(ctx, database)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.Get().StreamMode != mode || reloaded.Get().CoverMode != "redirect" {
				t.Fatalf("重载应保留播放方式，且不影响封面方式: %+v", reloaded.Get())
			}
		})
	}
}
