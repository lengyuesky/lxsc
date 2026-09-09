package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"lxsc/internal/db"
)

func TestTTLValidationAndAtomicPublication(t *testing.T) {
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
	for _, raw := range []string{`-2`, `1`, `900`, `1.5`, `1.0`, `1e2`, `true`, `null`, `[]`, `{}`, `""`, `"1.5"`, `9223372037`, `18446744073709551616`} {
		t.Run(raw, func(t *testing.T) {
			before := store.Get()
			_, err := store.Update(ctx, map[string]json.RawMessage{"urlCacheTTL": json.RawMessage(raw), "serverName": json.RawMessage(`"不应生效"`)})
			if !errors.Is(err, ErrInvalidSetting) {
				t.Fatalf("应拒绝 %s: %v", raw, err)
			}
			if !reflect.DeepEqual(before, store.Get()) || database.GetSetting(ctx, "serverName", "") != "" {
				t.Fatal("非法更新不能部分发布或落库")
			}
		})
	}
	for _, raw := range []string{`0`, `86400`, `"604800"`, `2592000`, `-1`} {
		if _, err := store.Update(ctx, map[string]json.RawMessage{"urlCacheTTL": json.RawMessage(raw)}); err != nil {
			t.Fatalf("合法 TTL 被拒绝 %s: %v", raw, err)
		}
	}
	if _, err := store.Update(ctx, map[string]json.RawMessage{"searchCacheTTL": json.RawMessage(`-1`)}); !errors.Is(err, ErrInvalidSetting) {
		t.Fatal("永久只适用于直链缓存")
	}
	for _, raw := range []string{`0`, `1`, `"2"`, `9223372036`} {
		if _, err := store.Update(ctx, map[string]json.RawMessage{"searchCacheTTL": json.RawMessage(raw)}); err != nil {
			t.Fatalf("搜索缓存应保留原秒数契约: %s %v", raw, err)
		}
	}
	before := store.Get()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(ctx, map[string]json.RawMessage{"urlCacheTTL": json.RawMessage(`0`), "serverName": json.RawMessage(`"数据库失败"`)})
	if err == nil || !reflect.DeepEqual(store.Get(), before) {
		t.Fatal("数据库失败不能改变内存快照")
	}
}

func TestURLCachePresetsMigrationAndReload(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   int
	}{
		{"", 604800}, {"900", 604800}, {"1234567", 604800}, {"1.5", 604800},
		{"0", 0}, {"86400", 86400}, {"604800", 604800}, {"2592000", 2592000}, {"-1", -1},
	} {
		name := tc.stored
		if name == "" {
			name = "默认设置"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if tc.stored != "" {
				if err := database.SetSettings(ctx, map[string]string{"urlCacheTTL": tc.stored, "searchCacheTTL": "123"}); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				store, err := New(ctx, database)
				if err != nil || store.Get().URLCacheTTL != tc.want {
					t.Fatalf("迁移与重载结果错误: %v %v", store, err)
				}
				if tc.stored != "" && (database.GetSetting(ctx, "urlCacheTTL", "") != fmt.Sprint(tc.want) || store.Get().SearchCacheTTL != 123) {
					t.Fatal("迁移必须落库，并只修改直链设置")
				}
			}
			store, _ := New(ctx, database)
			for _, value := range []int{0, 86400, 604800, 2592000, -1} {
				if _, err := store.Update(ctx, map[string]json.RawMessage{"urlCacheTTL": json.RawMessage(fmt.Sprint(value))}); err != nil {
					t.Fatal(err)
				}
				reloaded, err := New(ctx, database)
				if err != nil || reloaded.Get().URLCacheTTL != value {
					t.Fatalf("五档设置必须跨重启保留: %d %v", value, err)
				}
			}
		})
	}
}

func TestSettingsSnapshotsDoNotShareSlices(t *testing.T) {
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
	snapshot := store.Get()
	snapshot.SearchSources[0] = "changed"
	snapshot.BoardSources[0] = "changed"
	updated, err := store.Update(ctx, map[string]json.RawMessage{"searchSources": json.RawMessage(`["wy","tx"]`)})
	if err != nil {
		t.Fatal(err)
	}
	updated.SearchSources[0] = "changed"
	updated.BoardSources[0] = "changed"
	got := store.Get()
	if got.SearchSources[0] != "wy" || got.BoardSources[0] != "wy" {
		t.Fatal("Get/Update 返回值不得共享设置切片")
	}
}

func TestStoredTTLCompatibilityAndOverflow(t *testing.T) {
	v := apply(Defaults(), map[string]string{"urlCacheTTL": "0", "searchCacheTTL": "1"})
	if v.URLCacheTTL != 0 || v.SearchCacheTTL != 1 {
		t.Fatal("应兼容已有整数设置")
	}
	v = apply(v, map[string]string{"urlCacheTTL": "9223372037", "searchCacheTTL": "-1"})
	if v.URLCacheTTL != 0 || v.SearchCacheTTL != 1 {
		t.Fatal("已有非法值不得产生溢出")
	}
}
