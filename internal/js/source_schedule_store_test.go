package js

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/db"
)

func persistentScheduleManager(t *testing.T, database *db.DB, clock *atomic.Int64, scripts map[int64]string) *SourceManager {
	t.Helper()
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.schedule.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	t.Cleanup(m.UnloadAll)
	fingerprints := map[int64]string{}
	for id, script := range scripts {
		if _, err := m.Load(context.Background(), id, script); err != nil {
			t.Fatal(err)
		}
		fingerprints[id] = ScriptFingerprint(script)
	}
	if err := m.EnableSchedulePersistence(context.Background(), database, fingerprints); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.CloseSchedulePersistence(); err != nil {
			t.Error(err)
		}
	})
	return m
}

func TestSchedulePersistenceRestoresRankingAndAbsoluteDeadlines(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	broken := scheduleTestPrelude + `globalThis.__lx_request=()=>{throw new Error('故障')};`
	healthy := scheduleTestPrelude + `globalThis.__lx_request=()=>({url:'https://media.invalid/healthy'});`
	a, err := database.CreateSource(ctx, &db.Source{Name: "源A", Script: broken, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := database.CreateSource(ctx, &db.Source{Name: "源B", Script: healthy, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[int64]string{a.ID: broken, b.ID: healthy}
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC).UnixMilli())
	m := persistentScheduleManager(t, database, &clock, scripts)
	m.ReportMediaOutcome(a.ID, "wy", false)
	clock.Add(6000)
	m.ReportMediaOutcome(a.ID, "wy", false)
	result, err := m.MusicURLForSources(ctx, "wy", nil, "320k", []int64{b.ID})
	if err != nil || result.SourceID != b.ID {
		t.Fatalf("真实调用应返回健康音源：%+v %v", result, err)
	}
	m.BackoffMusicURL(a.ID, "wy", time.Minute)
	cooldown, retry := m.ScheduleSnapshot(a.ID)[0].CoolingUntil, m.ScheduleSnapshot(a.ID)[0].RetryUntil
	if err := m.CloseSchedulePersistence(); err != nil {
		t.Fatal(err)
	}
	m.UnloadAll()
	clock.Add(10000)
	restarted := persistentScheduleManager(t, database, &clock, scripts)
	if ids := restarted.MusicURLSourceIDs("wy"); len(ids) != 2 || ids[0] != b.ID {
		t.Fatalf("重启后应沿用有效健康排序：%v", ids)
	}
	state := restarted.ScheduleSnapshot(a.ID)[0]
	if state.Samples != 2 || state.ConsecutiveFailures != 2 || !state.CoolingUntil.Equal(cooldown) || !state.RetryUntil.Equal(retry) {
		t.Fatalf("不能重启续期或丢失失败计数：%+v", state)
	}
	if _, err := restarted.MusicURLForSources(ctx, "wy", nil, "320k", []int64{a.ID}); !errors.Is(err, ErrNoSource) {
		t.Fatal("尚未到期的限流等待必须恢复", err)
	}
	clock.Add(60000)
	clock.Add((scheduleSampleTTL + time.Second).Milliseconds())
	state = restarted.ScheduleSnapshot(a.ID)[0]
	if state.Samples != 0 || state.ConsecutiveFailures != 0 {
		t.Fatalf("长时间闲置后应清理陈旧样本及惩罚：%+v", state)
	}
	if ids := restarted.MusicURLSourceIDs("wy"); ids[0] != a.ID {
		t.Fatalf("全部样本过期后回到冷启动排序：%v", ids)
	}
}

func TestSchedulePersistenceReloadChangeDeleteAndSnapshot(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	script := scheduleTestPrelude + `globalThis.__lx_request=()=>({url:'https://media.invalid/fake-secret'});`
	source, err := database.CreateSource(ctx, &db.Source{Name: "音源", Script: script, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli())
	m := persistentScheduleManager(t, database, &clock, map[int64]string{source.ID: script})
	if _, err := m.MusicURL(ctx, "wy", nil, "320k"); err != nil {
		t.Fatal(err)
	}
	m.Unload(source.ID)
	if _, err := m.Load(ctx, source.ID, script); err != nil {
		t.Fatal(err)
	}
	if state := m.ScheduleSnapshot(source.ID); len(state) != 1 || state[0].Samples != 1 {
		t.Fatalf("相同脚本停用、启用与重载应保留经验：%+v", state)
	}
	if err := m.FlushSchedulePersistence(ctx); err != nil {
		t.Fatal(err)
	}
	records, err := database.SourceSchedules(ctx)
	if err != nil || strings.Contains(string(records[source.ID]), "fake-secret") || strings.Contains(string(records[source.ID]), "__lx_request") {
		t.Fatal("持久状态不能保存脚本正文或地址")
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := database.Snapshot(ctx, path); err != nil {
		t.Fatal(err)
	}
	backup, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backup.Close() })
	clock.Add(1000)
	restored := persistentScheduleManager(t, backup, &clock, map[int64]string{source.ID: script})
	if state := restored.ScheduleSnapshot(source.ID); len(state) != 1 || state[0].Samples != 1 {
		t.Fatalf("完整SQLite快照必须保留调度状态：%+v", state)
	}
	oldHash := ScriptFingerprint(script)
	changed := script + "\n// 内容发生变化"
	if _, err := m.Load(ctx, source.ID, changed); err != nil {
		t.Fatal(err)
	}
	if len(m.ScheduleSnapshot(source.ID)) != 0 {
		t.Fatal("脚本内容变化必须重新学习")
	}
	m.ReportMediaForScript(source.ID, "wy", oldHash, false)
	if len(m.ScheduleSnapshot(source.ID)) != 0 {
		t.Fatal("旧脚本的迟到反馈不能污染新脚本")
	}
	m.Unload(source.ID)
	if err := database.DeleteSource(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	m.ForgetSchedule(source.ID)
	if err := m.FlushSchedulePersistence(ctx); err != nil {
		t.Fatal(err)
	}
	if records, err := database.SourceSchedules(ctx); err != nil || len(records) != 0 {
		t.Fatalf("删除后不能重新写回状态：%v %v", records, err)
	}
}
