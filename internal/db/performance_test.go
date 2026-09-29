package db

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// BenchmarkListeningWithUserLookups 测量统计页面持续读取时鉴权查询的尾延迟。
// 合成数据只写入临时数据库，固定为一万次收听，便于比较前后改动。
func BenchmarkListeningWithUserLookups(b *testing.B) {
	d, err := Open(filepath.Join(b.TempDir(), "benchmark.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	u, err := d.CreateUser(ctx, "基准用户", "enc", false, "320k")
	if err != nil {
		b.Fatal(err)
	}
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, ListeningZone)
	_, err = d.sql.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10000)
INSERT INTO listening_sessions(user_id,session_key,source,track_id,name,singer,started_at)
SELECT ?,CAST(x AS TEXT),'web','tr-wy-'||(x%100),'基准歌曲','歌手',? FROM n`, u.ID, at.UnixMilli())
	if err != nil {
		b.Fatal(err)
	}
	_, err = d.sql.Exec(`INSERT INTO listening_days SELECT id,'2026-09-28',180000 FROM listening_sessions`)
	if err != nil {
		b.Fatal(err)
	}
	stop, done := make(chan struct{}), make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if _, err := d.ListeningStatistics(ctx, 0, 30, at); err != nil {
				done <- err
				return
			}
		}
	}()
	<-started
	latencies := make([]int64, b.N)
	before := d.sql.Stats()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if _, err := d.GetUserByID(ctx, u.ID); err != nil {
			b.Error(err)
			break
		}
		latencies[i] = time.Since(start).Nanoseconds()
	}
	b.StopTimer()
	close(stop)
	if err := <-done; err != nil {
		b.Fatal(err)
	}
	after := d.sql.Stats()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	b.ReportMetric(float64(latencies[(len(latencies)-1)*95/100])/1e6, "p95-ms")
	b.ReportMetric(float64(after.WaitDuration-before.WaitDuration)/float64(time.Millisecond), "db-wait-ms")
	b.ReportMetric(d.ListeningQueries.Snapshot().AverageMS, "statistics-average-ms")
}
