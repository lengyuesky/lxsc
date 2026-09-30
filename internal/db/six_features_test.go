package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestPlaylistHistorySurvivesCleanupAndIsPrivate(t *testing.T) {
	d, alice, bob := newPlaylistWriteDB(t)
	ctx := context.Background()
	old := []string{"tr-wy-1", "tr-wy-2", "tr-wy-1"}
	if err := d.CreatePlaylistWithMetadata(ctx, "pl-history", alice.ID, "历史", "", true, old, []Track{testPlaylistTrack(old[0]), testPlaylistTrack(old[1])}); err != nil {
		t.Fatal(err)
	}
	revision := TracksRevision(old)
	next := []string{"tr-tx-3"}
	if _, err := d.ReplacePlaylistTracksChecked(ctx, "pl-history", alice, next, []Track{testPlaylistTrack(next[0])}, &revision); err != nil {
		t.Fatal(err)
	}
	history, err := d.PlaylistHistory(ctx, "pl-history", alice)
	if err != nil || len(history) != 1 || history[0].Count != 3 {
		t.Fatalf("历史记录异常: %v %v", history, err)
	}
	ids, err := d.PlaylistHistoryTracks(ctx, "pl-history", history[0].ID, alice)
	if err != nil || !reflect.DeepEqual(ids, old) {
		t.Fatalf("原顺序或重复项丢失: %v %v", ids, err)
	}
	if _, err := d.PlaylistHistory(ctx, "pl-history", bob); !errors.Is(err, ErrPlaylistForbidden) {
		t.Fatalf("公开歌单的历史泄露: %v", err)
	}
	if _, err := d.PlaylistHistoryTracks(ctx, "pl-history", history[0].ID, bob); !errors.Is(err, ErrPlaylistForbidden) {
		t.Fatal("非归属用户可读取历史")
	}
	if _, err := d.CleanupUnreferencedMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetTrack(ctx, old[1]); err != nil {
		t.Fatal("历史引用的原歌曲被清理", err)
	}
	if _, err := d.ReplacePlaylistTracksChecked(ctx, "pl-history", alice, old, nil, &revision); !errors.Is(err, ErrPlaylistConflict) {
		t.Fatal("冲突应被拒绝", err)
	}
	history, _ = d.PlaylistHistory(ctx, "pl-history", alice)
	if len(history) != 1 {
		t.Fatal("冲突产生了历史记录")
	}
	for i := 0; i < 12; i++ {
		next = []string{fmt.Sprintf("tr-wy-%d", i+10)}
		if _, err := d.ReplacePlaylistTracksChecked(ctx, "pl-history", alice, next, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	history, err = d.PlaylistHistory(ctx, "pl-history", alice)
	if err != nil || len(history) != 10 {
		t.Fatalf("历史上限未生效: %d %v", len(history), err)
	}
	if err := d.DeletePlaylist(ctx, "pl-history"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM playlist_track_history`).Scan(&count); err != nil || count != 0 {
		t.Fatal("历史未级联删除", count, err)
	}
}
func TestPlaylistHistoryRollsBackWithFailedWrite(t *testing.T) {
	d, alice, _ := newPlaylistWriteDB(t)
	ctx := context.Background()
	if err := d.CreatePlaylistFull(ctx, "pl-history", alice.ID, "历史", "", false, []string{"tr-wy-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER fail_playlist BEFORE INSERT ON playlist_tracks BEGIN SELECT RAISE(ABORT,'失败测试'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReplacePlaylistTracksChecked(ctx, "pl-history", alice, []string{"tr-wy-2"}, nil, nil); err == nil {
		t.Fatal("预期写入失败")
	}
	history, err := d.PlaylistHistory(ctx, "pl-history", alice)
	if err != nil || len(history) != 0 {
		t.Fatal("失败保存留下了历史", err)
	}
	p, err := d.GetPlaylist(ctx, "pl-history")
	if err != nil || len(p.TrackIDs) != 1 || p.TrackIDs[0] != "tr-wy-1" {
		t.Fatal("失败保存改变了歌单", err)
	}
}
func TestSmartMonthUsesBeijingCalendar(t *testing.T) {
	for _, tc := range []struct{ at, kind, from, to string }{
		{"2026-09-30T16:01:00Z", "month", "2026-10-01", "2026-10-01"},
		{"2026-09-30T16:01:00Z", "frequent", "2026-09-02", "2026-10-01"},
		{"2026-01-01T00:00:00Z", "month", "2026-01-01", "2026-01-01"},
		{"2028-02-29T00:00:00Z", "month", "2028-02-01", "2028-02-29"},
	} {
		at, err := time.Parse(time.RFC3339, tc.at)
		if err != nil {
			t.Fatal(err)
		}
		from, to := smartDateRange(tc.kind, at)
		if from != tc.from || to != tc.to {
			t.Fatalf("%s %s: %s–%s", tc.kind, tc.at, from, to)
		}
	}
}
