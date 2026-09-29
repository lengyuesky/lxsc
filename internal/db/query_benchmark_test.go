package db

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkTrackSubstringSearch(b *testing.B) {
	for _, size := range []int{10000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d, err := Open(filepath.Join(b.TempDir(), "search.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			_, err = d.sql.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
   INSERT INTO tracks SELECT 'tr-wy-'||x,'wy',CASE WHEN x%1000=0 THEN '唯一匹配歌曲'||x ELSE '普通歌曲'||x END,'歌手','专辑','{}',x FROM n`, size)
			if err != nil {
				b.Fatal(err)
			}
			for _, indexed := range []bool{false, true} {
				name := "原模糊扫描"
				if indexed {
					name = "子串索引"
				}
				b.Run(name, func(b *testing.B) {
					query, args := trackSearchQuery("唯一匹配", 20, 0)
					if !indexed {
						query = `SELECT id,source,name,singer,album,json,updated_at FROM tracks WHERE name LIKE ? OR singer LIKE ? OR album LIKE ? ORDER BY updated_at DESC,id LIMIT ? OFFSET ?`
						args = []any{"%唯一匹配%", "%唯一匹配%", "%唯一匹配%", 20, 0}
					}
					b.ResetTimer()
					for range b.N {
						rows, err := d.read.Query(query, args...)
						if err != nil {
							b.Fatal(err)
						}
						var values [7]any
						dest := make([]any, 7)
						for i := range values {
							dest[i] = &values[i]
						}
						for rows.Next() {
							if err = rows.Scan(dest...); err != nil {
								b.Fatal(err)
							}
						}
						if err = rows.Err(); err != nil {
							b.Fatal(err)
						}
						rows.Close()
					}
				})
			}
		})
	}
}
func BenchmarkLibraryTrackIDs(b *testing.B) {
	d, err := Open(filepath.Join(b.TempDir(), "library.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	u, err := d.CreateUser(ctx, "基准", "enc", false, "320k")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		var ids []string
		for j := 0; j < 20; j++ {
			ids = append(ids, fmt.Sprintf("tr-wy-%d", i*20+j))
		}
		if err = d.CreatePlaylist(ctx, fmt.Sprint(i), u.ID, "歌单", ids); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("逐歌单读取", func(b *testing.B) {
		for range b.N {
			list, err := d.ListPlaylists(ctx, u.ID)
			if err != nil {
				b.Fatal(err)
			}
			for _, p := range list {
				if _, err = d.GetPlaylist(ctx, p.ID); err != nil {
					b.Fatal(err)
				}
			}
			if _, err = d.RecentTracks(ctx, u.ID, 200); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("批量读取", func(b *testing.B) {
		for range b.N {
			ids, err := d.LibraryTrackIDs(ctx, u.ID)
			if err != nil || len(ids) != 2000 {
				b.Fatal(len(ids), err)
			}
		}
	})
}
