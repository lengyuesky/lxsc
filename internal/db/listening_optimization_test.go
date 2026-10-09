package db

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestListeningTotalsIncludeTracksOutsideRanking(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	for i := 1; i <= 12; i++ {
		track := ListeningTrack{ID: fmt.Sprintf("tr-wy-%d", i), Name: fmt.Sprintf("歌曲%d", i)}
		if err := d.AddClientListening(ctx, user, "客户端", track, int64(i*1000), at.UnixMilli(), true, at); err != nil {
			t.Fatal(err)
		}
	}
	track := ListeningTrack{ID: "tr-wy-1", Name: "歌曲1"}
	p := ListeningProgress{UserID: user, SessionID: "cross-midnight", TrackID: track.ID, StartedAt: at.Add(-2 * time.Minute).UnixMilli(), Days: map[string]int64{"2026-09-07": 10000, "2026-09-08": 5000}}
	if err := d.SaveListeningProgress(ctx, p, track, at); err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateUser(ctx, "另一用户", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AddClientListening(ctx, other.ID, "客户端", track, 0, at.UnixMilli(), true, at); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{user, 0} {
		stats := listeningStats(t, d, id, at)
		plays, unknown := 13, 0
		if id == 0 {
			plays, unknown = 14, 1
		}
		if stats.TotalMS != 93000 || stats.WebMS != 15000 || stats.ClientMS != 78000 || stats.Plays != plays || stats.UnknownDuration != unknown || stats.Tracks != 12 || stats.ActiveDays != 2 || stats.AverageMS != 31000 {
			t.Fatalf("总计必须包含排行以外的歌曲且跨日会话只计一次: %+v", stats)
		}
		if len(stats.TopTracks) != 10 || stats.TopTracks[0].ID != track.ID || stats.TopTracks[0].TotalMS != 16000 {
			t.Fatalf("排行或重复歌曲聚合发生变化: %+v", stats.TopTracks)
		}
		if id == 0 && (len(stats.TopUsers) != 2 || stats.TopUsers[1].UnknownDuration != 1) {
			t.Fatalf("全站排行丢失未知时长用户: %+v", stats.TopUsers)
		}
	}
	var sessions, days int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM listening_sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM listening_days`).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if sessions != 14 || days != 15 {
		t.Fatal("统计查询不能清理原始会话或去重数据")
	}
}

// 固定用户数、日期与歌曲分布，比较优化前后的真实数据库聚合耗时。
func BenchmarkListeningStatistics(b *testing.B) {
	for _, size := range []int{10000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d, err := Open(filepath.Join(b.TempDir(), "listening.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer d.Close()
			ctx := context.Background()
			for i := range 7 {
				if _, err := d.CreateUser(ctx, fmt.Sprintf("用户%d", i), "enc", false, "320k"); err != nil {
					b.Fatal(err)
				}
			}
			_, err = d.sql.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO listening_sessions(user_id,session_key,source,track_id,name,singer,started_at,unknown_duration)
SELECT x%7+1,CAST(x AS TEXT),CASE WHEN x%2=0 THEN 'web' ELSE 'client' END,'tr-wy-'||(x%997),'歌曲'||(x%997),'歌手',1,x%19=0 FROM n`, size)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := d.sql.Exec(`INSERT INTO listening_days SELECT id,date('2026-09-28','-'||(id%30)||' days'),CASE WHEN unknown_duration=1 THEN 0 ELSE 180000 END FROM listening_sessions`); err != nil {
				b.Fatal(err)
			}
			at := time.Date(2026, 9, 28, 12, 0, 0, 0, ListeningZone)
			for _, scope := range []struct {
				name string
				user int64
			}{{"个人", 1}, {"全站", 0}} {
				b.Run(scope.name, func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						if _, err := d.ListeningStatistics(ctx, scope.user, 30, at); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
