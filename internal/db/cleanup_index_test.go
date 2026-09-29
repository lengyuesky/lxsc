package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMetadataReferenceQueriesUseIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟旧库升级，重复初始化也必须安全补齐索引。
	for _, index := range []string{"idx_playlist_tracks_track", "idx_stars_kind_item", "idx_history_track"} {
		if _, err := d.sql.Exec("DROP INDEX " + index); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, query := range []string{
		`SELECT 1 FROM playlist_tracks WHERE track_id='tr-wy-test'`,
		`SELECT 1 FROM stars WHERE kind='track' AND item_id='tr-wy-test'`,
		`SELECT 1 FROM history WHERE track_id='tr-wy-test'`,
	} {
		rows, err := d.sql.Query("EXPLAIN QUERY PLAN " + query)
		if err != nil {
			t.Fatal(err)
		}
		var details string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details += detail
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !strings.Contains(details, "SEARCH") || !strings.Contains(details, "INDEX") {
			t.Fatalf("查询未使用索引: %s: %s %v", query, details, err)
		}
	}
}
