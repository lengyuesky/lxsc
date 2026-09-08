package db

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func listeningFixture(t *testing.T) (*DB, int64, time.Time) {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	u, err := d.CreateUser(context.Background(), "用户", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 8, 0, 1, 0, 0, ListeningZone)
	if _, err = d.sql.Exec(`UPDATE listening_meta SET enabled_at=?`, at.Add(-25*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return d, u.ID, at
}
func listeningStats(t *testing.T, d *DB, userID int64, at time.Time) ListeningStats {
	t.Helper()
	stats, err := d.ListeningStatistics(context.Background(), userID, 30, at)
	if err != nil {
		t.Fatal(err)
	}
	return stats
}
func TestListeningProgressRetryMidnightAndValidation(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	track := ListeningTrack{ID: "tr-wy-1", Name: "歌曲", Singer: "歌手"}
	p := ListeningProgress{UserID: user, SessionID: "web-session-1", TrackID: track.ID, StartedAt: at.Add(-2 * time.Minute).UnixMilli(), Days: map[string]int64{"2026-09-07": 30000, "2026-09-08": 20000}}
	for range 2 {
		if err := d.SaveListeningProgress(ctx, p, track, at); err != nil {
			t.Fatal(err)
		}
	}
	p.Days["2026-09-08"] = 10000
	if err := d.SaveListeningProgress(ctx, p, track, at); err != nil {
		t.Fatal(err)
	}
	stats := listeningStats(t, d, user, at)
	if stats.WebMS != 50000 || stats.Plays != 1 || stats.Tracks != 1 || stats.ActiveDays != 2 || stats.CountedDays != 3 || stats.AverageMS != 16666 {
		t.Fatalf("跨日或幂等统计错误：%+v", stats)
	}
	if stats.Daily[28].WebMS != 30000 || stats.Daily[29].WebMS != 20000 {
		t.Fatal("每日时长归日错误")
	}
	for _, change := range []func(*ListeningProgress){
		func(p *ListeningProgress) { p.StartedAt++ },
		func(p *ListeningProgress) { p.Days = map[string]int64{"2026-09-08": 60001} },
		func(p *ListeningProgress) { p.Days = map[string]int64{"2026-09-09": 1} },
		func(p *ListeningProgress) { p.Days = map[string]int64{"2026-09-06": 1} },
		func(p *ListeningProgress) { p.Days = map[string]int64{"2026-9-8": 1} },
		func(p *ListeningProgress) { p.Days = map[string]int64{"2026-09-08": -1} },
	} {
		invalid := p
		change(&invalid)
		if err := d.SaveListeningProgress(ctx, invalid, track, at); !errors.Is(err, ErrListeningProgress) {
			t.Fatalf("应拒绝错误进度：%+v，%v", invalid, err)
		}
	}
	if listeningStats(t, d, user, at).TotalMS != 50000 {
		t.Fatal("拒绝上报后数据应保持原值")
	}
}
func TestListeningConcurrentRetryAndSnapshots(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	track := ListeningTrack{ID: "tr-wy-1", Name: "保留歌曲名", Singer: "保留歌手"}
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Go(func() {
			p := ListeningProgress{UserID: user, SessionID: "web-concurrent", TrackID: track.ID, StartedAt: at.Add(-time.Minute).UnixMilli(), Days: map[string]int64{"2026-09-08": int64(i) * 1000}}
			if err := d.SaveListeningProgress(ctx, p, track, at); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	stats := listeningStats(t, d, user, at)
	if stats.WebMS != 20000 || stats.Plays != 1 {
		t.Fatalf("并发重试重复计时：%+v", stats)
	}
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	if err := d.Snapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	copyDB, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	copyStats := listeningStats(t, copyDB, user, at)
	copyDB.Close()
	copyDB, err = Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	if copyStats.EnabledAt != stats.EnabledAt || listeningStats(t, copyDB, user, at).TotalMS != 20000 || copyStats.TopTracks[0].Name != track.Name {
		t.Fatal("备份或重启丢失统计、启用时间或歌曲快照")
	}
	if err := d.DeleteUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if listeningStats(t, d, 0, at).TotalMS != 0 {
		t.Fatal("删除用户后应清理统计")
	}
	var days int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM listening_days`).Scan(&days); err != nil || days != 0 {
		t.Fatal("每日记录未级联删除")
	}
}
func TestListeningClientDedupMissingDurationAndNoHistoryBackfill(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	track := ListeningTrack{ID: "tr-wy-1", Name: "歌曲"}
	if err := d.AddHistory(ctx, user, "tr-wy-old", at.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	enabled, _ := d.ListeningEnabledAt(ctx)
	for _, stamp := range []int64{enabled - 1, at.Add(time.Hour).UnixMilli()} {
		if err := d.AddClientListening(ctx, user, "音流", track, 180000, stamp, true, at); err != nil {
			t.Fatal(err)
		}
	}
	if listeningStats(t, d, user, at).Plays != 0 {
		t.Fatal("旧历史、早于启用和未来上报不应进入统计")
	}
	for range 2 {
		if err := d.AddClientListening(ctx, user, "音流", track, 180000, at.UnixMilli(), true, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddClientListening(ctx, user, "其他客户端", track, 180000, at.UnixMilli(), true, at); err != nil {
		t.Fatal(err)
	}
	// 原始毫秒不能先截断到秒后去重。
	if err := d.AddClientListening(ctx, user, "音流", track, 180000, at.UnixMilli()-1, true, at); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.AddClientListening(ctx, user, "无时间戳", ListeningTrack{ID: "tr-wy-2", Name: "未知时长"}, 0, at.UnixMilli(), false, at); err != nil {
			t.Fatal(err)
		}
	}
	stats := listeningStats(t, d, user, at)
	if stats.ClientMS != 540000 || stats.WebMS != 0 || stats.Plays != 5 || stats.UnknownDuration != 2 || stats.Tracks != 2 {
		t.Fatalf("客户端统计错误：%+v", stats)
	}
	other, err := d.CreateUser(ctx, "其他用户", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	if listeningStats(t, d, other.ID, at).Plays != 0 {
		t.Fatal("个人查询混入其他用户")
	}
	if len(listeningStats(t, d, 0, at).TopUsers) != 1 || len(stats.TopUsers) != 0 {
		t.Fatal("用户排行只能返回全站视图")
	}
}
func TestListeningOldDatabaseUpgrade(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	if err := d.AddHistory(ctx, user, "tr-wy-old", at.Unix()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"listening_days", "listening_sessions", "listening_meta"} {
		if _, err := d.sql.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	file := d.path
	d.Close()
	reopened, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stats := listeningStats(t, reopened, user, time.Now())
	if stats.Plays != 0 || stats.EnabledAt <= 0 {
		t.Fatal("升级应启用统计但不回填历史")
	}
	ids, err := reopened.RecentTracks(ctx, user, 10)
	if err != nil || len(ids) != 1 {
		t.Fatal("升级应保留播放历史")
	}
}
