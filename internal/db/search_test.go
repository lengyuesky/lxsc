package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTrackSearchUpgradeAndSynchronization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Exec(`INSERT INTO tracks VALUES('a','wy','晴天周杰伦','歌手','专辑','{}',1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.Close() }()
	ctx := context.Background()
	check := func(q string, want int) {
		t.Helper()
		got, err := d.SearchTracks(ctx, q, 20, 0)
		if err != nil || len(got) != want {
			t.Fatalf("搜索 %q：得到 %d，期望 %d，错误 %v", q, len(got), want, err)
		}
	}
	check("周杰伦", 1)
	if err = d.UpsertTracks(ctx, []Track{{ID: "a", Source: "wy", Name: "修改后的歌曲", JSON: []byte(`{}`)}, {ID: "b", Source: "tx", Name: "另一首歌曲", JSON: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	check("周杰伦", 0)
	check("修改后", 1)
	check("另一首", 1)
	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tracks SET name='回滚后的歌曲' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	check("回滚后", 0)
	check("修改后", 1)
	if _, err = d.sql.Exec(`DELETE FROM tracks WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	check("修改后", 0)
	if err = d.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	check("另一首", 1)
	if err = d.UpsertTracks(ctx, []Track{{ID: "b", Source: "tx", Name: "压缩后更新", JSON: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	check("另一首", 0)
	check("压缩后", 1)
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	check("压缩后", 1)
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err = d.Snapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	copy, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if found, err := copy.SearchTracks(ctx, "压缩后", 20, 0); err != nil || len(found) != 1 {
		t.Fatal("快照索引不可用", err)
	}
}

func TestTrackSearchKeepsSubstringSemanticsAndPaging(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	names := []string{"晴天周杰伦", "夏天周杰伦", "周杰", "A_B%歌名", "Hello WORLD", "你好🙂世界", "名字含\"引号", "Cafe café", "e\u0301clair", "三个 空格 词", "这是歌手名", "这是专辑名"}
	for i, name := range names {
		tr := Track{ID: fmt.Sprintf("t%02d", i), Source: "wy", Name: name, JSON: []byte(`{}`)}
		if i == 10 {
			tr.Name = "歌"
			tr.Singer = name
		}
		if i == 11 {
			tr.Name = "歌"
			tr.Album = name
		}
		if err := d.UpsertTracks(ctx, []Track{tr}); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"周杰伦", "周杰", "周", "A_B", "%歌", "world", "好🙂世", "含\"引", "café", "e\u0301c", " 空格 ", "歌手名", "专辑名", "不存在", "", "🙂"} {
		like := "%" + q + "%"
		rows, err := d.sql.Query(`SELECT id FROM tracks WHERE name LIKE ? OR singer LIKE ? OR album LIKE ? ORDER BY updated_at DESC,id`, like, like, like)
		if err != nil {
			t.Fatal(err)
		}
		var expected []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			expected = append(expected, id)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		got, err := d.SearchTracks(ctx, q, 100, 0)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, tr := range got {
			ids = append(ids, tr.ID)
		}
		if !reflect.DeepEqual(ids, expected) {
			t.Errorf("搜索 %q 改变语义：%v，期望 %v", q, ids, expected)
		}
	}
	first, _ := d.SearchTracks(ctx, "", 1, 0)
	second, _ := d.SearchTracks(ctx, "", 1, 1)
	if first[0].ID == second[0].ID {
		t.Fatal("分页重复")
	}
}

func TestTrackSearchQueryPlans(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{"周杰伦", ""} {
		query, args := trackSearchQuery(q, 20, 0)
		rows, err := d.sql.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var a, b, c int
			var detail string
			if err = rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail + "\n"
		}
		rows.Close()
		if q != "" && !strings.Contains(plan, "VIRTUAL TABLE INDEX") {
			t.Fatal("长关键词未使用子串索引", plan)
		}
		if q == "" && (!strings.Contains(plan, "idx_tracks_updated_id") || strings.Contains(plan, "TEMP B-TREE")) {
			t.Fatal("空搜索应按索引分页", plan)
		}
	}
}
