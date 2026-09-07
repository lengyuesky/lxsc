package settings

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"lxsc/internal/db"
)

func TestBoardSelectionsPersistAndReplace(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Get().BoardSelections; got == nil || len(got) != 0 {
		t.Fatalf("旧配置应默认展示全部榜单: %#v", got)
	}
	want := map[string][]string{"wy": {"one", "two"}, "tx": {}}
	updated, err := store.Update(ctx, map[string]json.RawMessage{"boardSelections": json.RawMessage(`{"wy":[" one ","two","one"],"tx":[]}`)})
	if err != nil || !reflect.DeepEqual(updated.BoardSelections, want) {
		t.Fatalf("榜单选择应规范化并保留空数组: %+v %v", updated.BoardSelections, err)
	}
	// 调用者对嵌套快照的修改不能影响内存设置。
	updated.BoardSelections["wy"][0] = "changed"
	delete(updated.BoardSelections, "tx")
	snapshot := store.Get()
	snapshot.BoardSelections["wy"][1] = "changed"
	snapshot.BoardSelections["kg"] = []string{"changed"}
	if _, err := store.Update(ctx, map[string]json.RawMessage{"showBoards": json.RawMessage(`false`), "boardSources": json.RawMessage(`["tx"]`)}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(ctx, database)
	if err != nil || !reflect.DeepEqual(reloaded.Get().BoardSelections, want) || !reflect.DeepEqual(store.Get().BoardSelections, want) {
		t.Fatalf("关闭开关、停用平台或重启均不能清除选择: %v", err)
	}
	if got := database.GetSetting(ctx, "boardSelections", ""); got != `{"tx":[],"wy":["one","two"]}` {
		t.Fatalf("空选择必须持久化为 []: %s", got)
	}
	if _, err := store.Update(ctx, map[string]json.RawMessage{"boardSelections": json.RawMessage(`{"kg":["missing"]}`)}); err != nil {
		t.Fatal(err)
	}
	if got := store.Get().BoardSelections; !reflect.DeepEqual(got, map[string][]string{"kg": {"missing"}}) {
		t.Fatalf("提供榜单选择时应整体替换，并允许暂时未返回的 ID: %+v", got)
	}
	if _, err := store.Update(ctx, map[string]json.RawMessage{"boardSelections": json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if len(store.Get().BoardSelections) != 0 {
		t.Fatal("空对象应恢复全部平台的默认展示")
	}
}

func TestInvalidBoardSelectionsAreAtomic(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `[]`, `true`, `"{}"`, `{"other":[]}`, `{"wy":null}`, `{"wy":"one"}`, `{"wy":[1]}`, `{"wy":[null]}`, `{"wy":[""]}`, `{"wy":["  "]}`, `{"wy":["0"]}`} {
		t.Run(raw, func(t *testing.T) {
			before := store.Get()
			_, err := store.Update(ctx, map[string]json.RawMessage{"boardSelections": json.RawMessage(raw), "serverName": json.RawMessage(`"不应保存"`)})
			if !errors.Is(err, ErrInvalidSetting) || !reflect.DeepEqual(before, store.Get()) {
				t.Fatalf("非法榜单选择不能部分更新: %v", err)
			}
			if database.GetSetting(ctx, "serverName", "") != "" || database.GetSetting(ctx, "boardSelections", "") != "" {
				t.Fatal("非法设置不能落库")
			}
		})
	}
	before := store.Get()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(ctx, map[string]json.RawMessage{"boardSelections": json.RawMessage(`{"wy":[]}`)})
	if err == nil || !reflect.DeepEqual(before, store.Get()) {
		t.Fatal("数据库写入失败不能发布新选择")
	}
}

func TestBoardSelectionSnapshotsConcurrent(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	patch := map[string]json.RawMessage{"boardSelections": json.RawMessage(`{"wy":["one"],"tx":[]}`)}
	if _, err := store.Update(ctx, patch); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 30 {
				snapshot := store.Get()
				snapshot.BoardSelections["wy"][0] = "changed"
				delete(snapshot.BoardSelections, "tx")
			}
		})
	}
	for range 10 {
		if _, err := store.Update(ctx, patch); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if store.Get().BoardSelections["wy"][0] != "one" {
		t.Fatal("并发读取和修改快照不能改变已保存选择")
	}
}
