package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func playbackStart(user int64, key, client string, at time.Time) PlaybackStart {
	return PlaybackStart{UserID: user, UserName: "测试用户", Key: key, Protocol: "web", Client: client, Kind: "playback", Track: PlaybackTrack{ID: "tr-wy-1", Name: "歌曲_100%", Singer: "歌手", Platform: "wy"}, At: at.UnixMilli()}
}

func TestRecentPlaybackAggregatesRequestsAndKeepsActualSource(t *testing.T) {
	d, alice, at := listeningFixture(t)
	ctx := context.Background()
	bob, err := d.CreateUser(ctx, "日志用户B", "fake", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	p := playbackStart(alice, "same-playback", "网页播放器", at)
	first, err := d.BeginPlayback(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.BeginPlayback(ctx, p)
	if err != nil || first.ID != second.ID || second.Sequence != 2 {
		t.Fatalf("重试应归入同条日志：%+v %+v %v", first, second, err)
	}
	result := PlaybackResult{Track: p.Track, Platform: "kg", ResolvedTrackID: "tr-kg-alternate", SourceID: 20, SourceName: "成功音源", RequestedQuality: "flac24bit", Quality: "320k", Mode: "proxy", Delivery: "proxy", Outcome: "success", Status: 206, Cached: true}
	events := []PlaybackEvent{{At: at.UnixMilli(), Stage: "resolve", SourceID: 10, SourceName: "失败音源", Platform: "wy", Error: "timeout"}, {At: at.UnixMilli(), Stage: "cross_source", SourceID: 20, SourceName: "成功音源", Platform: "kg", Quality: "320k", Status: 206}}
	for range 2 {
		if err := d.CompletePlayback(ctx, second, result, events, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.CompletePlayback(ctx, first, PlaybackResult{Track: p.Track, Outcome: "failed"}, []PlaybackEvent{{At: at.UnixMilli(), Stage: "resolve", Error: "cancelled"}}, at); err != nil {
		t.Fatal(err)
	}
	bobStart := playbackStart(bob.ID, "same-playback", "其他用户的私有客户端", at)
	bobLog, err := d.BeginPlayback(ctx, bobStart)
	if err != nil || bobLog.ID == first.ID {
		t.Fatalf("播放标识必须按用户隔离：%+v %v", bobLog, err)
	}
	page, err := d.RecentPlayback(ctx, PlaybackQuery{UserID: alice, SourceID: 10, Query: "%"}, at)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("失败音源筛选与字面百分号搜索错误：%+v %v", page, err)
	}
	log := page.Items[0]
	if log.Outcome != "success" || log.SourceID != 20 || log.RequestedPlatform != "wy" || log.Platform != "kg" || log.Delivery != "proxy" || log.Status != 206 || log.Requests != 2 || log.DetailCount != 3 {
		t.Fatalf("迟到请求不能覆盖最终来源，重复保存不增加明细：%+v", log)
	}
	if strings.Contains(strings.Join(page.Clients, ","), "私有") {
		t.Fatal("筛选选项泄漏其他用户的客户端")
	}
	if _, err := d.PlaybackDetail(ctx, bobLog.ID, alice, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不能通过明细编号越权：%v", err)
	}
	detail, err := d.PlaybackDetail(ctx, first.ID, alice, at)
	if err != nil || len(detail.Events) != 3 || detail.ResolvedTrackID != "tr-kg-alternate" {
		t.Fatalf("明细与最终曲目错误：%+v %v", detail, err)
	}
	if err := d.CompletePlayback(ctx, PlaybackHandle{ID: first.ID, UserID: bob.ID, Sequence: 3}, result, nil, at); !errors.Is(err, ErrPlaybackLog) {
		t.Fatal("错误归属不能修改日志")
	}
	p.Track.ID = "tr-wy-2"
	if _, err := d.BeginPlayback(ctx, p); !errors.Is(err, ErrPlaybackLog) {
		t.Fatal("同一播放标识不能改成另一首歌曲")
	}
}

func TestRecentPlaybackCursorIsStableAndDetailsAreBounded(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	for _, key := range []string{"playback-a", "playback-b", "playback-c"} {
		if _, err := d.BeginPlayback(ctx, playbackStart(user, key, "客户端", at)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := d.RecentPlayback(ctx, PlaybackQuery{UserID: user, Limit: 1}, at)
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("首页分页错误：%+v %v", first, err)
	}
	// 首页读取后新增日志，不会令旧游标重读首页。
	if _, err := d.BeginPlayback(ctx, playbackStart(user, "new-playback", "客户端", at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	second, err := d.RecentPlayback(ctx, PlaybackQuery{UserID: user, Limit: 1, Cursor: first.NextCursor}, at.Add(time.Second))
	if err != nil || len(second.Items) != 1 || second.Items[0].ID >= first.Items[0].ID {
		t.Fatalf("并发新增不能扰乱游标：%+v %v", second, err)
	}
	if _, err := d.RecentPlayback(ctx, PlaybackQuery{UserID: user, Cursor: "invalid-cursor"}, at); !errors.Is(err, ErrPlaybackLog) {
		t.Fatal("损坏游标必须拒绝")
	}
	p := playbackStart(user, "bounded-details", "客户端", at)
	var id int64
	for request := range 3 {
		h, err := d.BeginPlayback(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		id = h.ID
		events := make([]PlaybackEvent, 60)
		for index := range events {
			events[index] = PlaybackEvent{At: at.UnixMilli(), Stage: "resolve", SourceID: int64(request + 1)}
		}
		if err := d.CompletePlayback(ctx, h, PlaybackResult{Track: p.Track, Outcome: "success"}, events, at); err != nil {
			t.Fatal(err)
		}
	}
	detail, err := d.PlaybackDetail(ctx, id, user, at)
	if err != nil || detail.DetailCount != 180 || len(detail.Events) != PlaybackDetailLimit || detail.Events[len(detail.Events)-1].Request != 3 {
		t.Fatalf("只保留最近明细，同时保留总数量：%+v %v", detail, err)
	}
}

func TestPlaybackRetentionSnapshotAndClientStatisticsAreIndependent(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	old, err := d.BeginPlayback(ctx, playbackStart(user, "old-playback", "客户端", at.Add(-31*24*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompletePlayback(ctx, old, PlaybackResult{Outcome: "failed"}, []PlaybackEvent{{Stage: "resolve", At: at.UnixMilli()}}, at); err != nil {
		t.Fatal(err)
	}
	p := playbackStart(user, "fn-playback-1", "飞牛音乐", at)
	p.Protocol, p.Kind = "fnmusic", "playback"
	for range 3 {
		if err := d.RecordFnPlayback(ctx, p, 180000); err != nil {
			t.Fatal(err)
		}
	}
	p.Key = "fn-playback-2"
	if err := d.RecordFnPlayback(ctx, p, 0); err != nil {
		t.Fatal(err)
	}
	stats := listeningStats(t, d, user, at)
	if stats.ClientMS != 180000 || stats.Plays != 2 || stats.UnknownDuration != 1 {
		t.Fatalf("客户端估算应幂等，缺少时长仅计次数：%+v", stats)
	}
	if n, err := d.CleanupPlayback(ctx, at); err != nil || n != 1 {
		t.Fatalf("应只清理过期日志：%d %v", n, err)
	}
	var events int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM playback_events WHERE log_id=?`, old.ID).Scan(&events); err != nil || events != 0 {
		t.Fatal("清理日志必须级联清理其明细")
	}
	if after := listeningStats(t, d, user, at); after.ClientMS != stats.ClientMS || after.Plays != stats.Plays {
		t.Fatal("日志清理不能影响累计听歌统计")
	}
	backup := filepath.Join(t.TempDir(), "snapshot.db")
	if err := d.Snapshot(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	page, err := restored.RecentPlayback(ctx, PlaybackQuery{UserID: user}, at)
	if err != nil || len(page.Items) != 2 || listeningStats(t, restored, user, at).Plays != 2 {
		t.Fatalf("快照恢复必须包含日志与统计：%+v %v", page, err)
	}
}

func TestRecoverPlaybackDoesNotLeaveOldTransfersRunning(t *testing.T) {
	d, user, at := listeningFixture(t)
	ctx := context.Background()
	p := playbackStart(user, "previous-process", "网页播放器", at)
	h, err := d.BeginPlayback(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompletePlayback(ctx, h, PlaybackResult{Track: p.Track, SourceID: 1, SourceName: "音源", Delivery: "proxy", Outcome: "pending", Status: 200}, nil, at); err != nil {
		t.Fatal(err)
	}
	if err := d.RecoverPlayback(ctx, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	detail, err := d.PlaybackDetail(ctx, h.ID, user, at.Add(time.Minute))
	if err != nil || detail.Outcome != "unknown" || detail.SourceID != 1 || detail.Delivery != "proxy" {
		t.Fatalf("旧进程未结束的传输必须标为结果未确认并保留来源：%+v %v", detail, err)
	}
	if stats := listeningStats(t, d, user, at.Add(time.Minute)); stats.Plays != 0 || stats.TotalMS != 0 {
		t.Fatal("恢复日志不能据此虚构收听统计")
	}
}
