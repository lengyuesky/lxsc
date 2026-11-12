package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestAuthorizedTrackWritesRecheckSourceBeforeCreatingReferences(t *testing.T) {
	d, alice, bob := newPlaylistWriteDB(t)
	ctx := context.Background()
	row := testPlaylistTrack("tr-wy-shared")
	if err := d.CreatePlaylistWithMetadata(ctx, "shared", alice.ID, "公开", "", true, []string{row.ID}, []Track{row}); err != nil {
		t.Fatal(err)
	}
	_ = d.CreatePlaylist(ctx, "target", bob.ID, "目标", nil)
	authority := TrackWriteAuthority{Actor: bob, LibraryIDs: []string{row.ID}}
	allowed, _ := d.CanReadLibraryTrack(ctx, bob, row.ID)
	if !allowed {
		t.Fatal("夹具前置读取权限错误")
	}
	no := false
	_ = d.UpdatePlaylistMeta(ctx, "shared", nil, nil, &no)
	mapping := ClientID{Protocol: "fnmusic", Kind: "track", ID: row.ID, GUID: "11111111111111111111111111111111"}
	if err := d.StarTracksAuthorized(ctx, bob.ID, []Track{row}, authority, mapping); !errors.Is(err, ErrNotFound) {
		t.Fatal("已撤销源权限仍可收藏", err)
	}
	if _, err := d.PrependPlaylistTracksAuthorized(ctx, "target", bob, []Track{row}, authority, mapping); !errors.Is(err, ErrNotFound) {
		t.Fatal("已撤销源权限仍可创建歌单引用", err)
	}
	stars, _ := d.ListStarred(ctx, bob.ID, "track")
	playlist, _ := d.GetPlaylist(ctx, "target")
	if len(stars) != 0 || playlist.Count != 0 {
		t.Fatal("授权失败产生部分写入")
	}
	if _, _, err := d.ResolveClientID(ctx, mapping.Protocol, mapping.GUID); !errors.Is(err, ErrNotFound) {
		t.Fatal("授权失败仍保存客户端标识")
	}
}

func TestMetadataRelationAndMappingFailuresRollBackTogether(t *testing.T) {
	d, alice, _ := newPlaylistWriteDB(t)
	ctx := context.Background()
	_ = d.CreatePlaylist(ctx, "target", alice.ID, "目标", []string{"old"})
	mapping := ClientID{Protocol: "fnmusic", Kind: "track", ID: "conflicting", GUID: "11111111111111111111111111111111"}
	if err := d.EnsureClientID(ctx, mapping.Protocol, mapping.Kind, "different", mapping.GUID); err != nil {
		t.Fatal(err)
	}
	rows := []Track{testPlaylistTrack(mapping.ID)}
	if err := d.StarTracksWithMetadata(ctx, alice.ID, rows, mapping); err == nil {
		t.Fatal("标识冲突应失败")
	}
	if _, err := d.PrependPlaylistTracks(ctx, "target", alice, rows, mapping); err == nil {
		t.Fatal("歌单标识冲突应失败")
	}
	if err := d.CreatePlaylistWithMetadata(ctx, "new", alice.ID, "新建", "", false, []string{mapping.ID}, rows, mapping); err == nil {
		t.Fatal("创建标识冲突应整体失败")
	}
	if _, err := d.GetTrack(ctx, mapping.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("回滚留下元数据")
	}
	playlist, _ := d.GetPlaylist(ctx, "target")
	if !reflect.DeepEqual(playlist.TrackIDs, []string{"old"}) {
		t.Fatal("失败修改了原有列表")
	}
	if _, err := d.GetPlaylist(ctx, "new"); !errors.Is(err, ErrNotFound) {
		t.Fatal("创建失败留下空歌单")
	}
}

func TestConcurrentBatchAddsPreserveEveryTrackAndOrder(t *testing.T) {
	d, alice, _ := newPlaylistWriteDB(t)
	ctx := context.Background()
	_ = d.CreatePlaylist(ctx, "target", alice.ID, "并发", []string{"old"})
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			rows := []Track{testPlaylistTrack(fmt.Sprintf("batch-%d-a", i)), testPlaylistTrack(fmt.Sprintf("batch-%d-b", i))}
			if _, err := d.PrependPlaylistTracks(ctx, "target", alice, rows); err != nil {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	p, _ := d.GetPlaylist(ctx, "target")
	if p.Count != 25 || p.TrackIDs[24] != "old" {
		t.Fatal("并发增量加歌丢失原有成员", p.TrackIDs)
	}
	positions := map[string]int{}
	for i, id := range p.TrackIDs {
		positions[id] = i
	}
	for i := range 12 {
		if positions[fmt.Sprintf("batch-%d-b", i)] != positions[fmt.Sprintf("batch-%d-a", i)]+1 {
			t.Fatal("批量请求的选中顺序被破坏")
		}
	}
}

func TestCoverExpiryAndUserCascadePreservePlaylistFallback(t *testing.T) {
	d, alice, bob := newPlaylistWriteDB(t)
	ctx := context.Background()
	for i := range 10 {
		if err := d.SavePlaylistCover(ctx, fmt.Sprintf("cover-%d", i), alice.ID, "image/png", []byte("fixture")); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(d.SavePlaylistCover(ctx, "extra", alice.ID, "image/png", nil), ErrCoverLimit) {
		t.Fatal("待使用封面上限无效")
	}
	_, _ = d.sql.ExecContext(ctx, `UPDATE playlist_covers SET created_at=?`, time.Now().Add(-25*time.Hour).Unix())
	if err := d.SavePlaylistCover(ctx, "new", alice.ID, "image/png", []byte("fixture")); err != nil {
		t.Fatal("旧孤立封面没有自动清理", err)
	}
	_ = d.CreatePlaylist(ctx, "bob-pl", bob.ID, "保留", nil)
	admin := *alice
	admin.IsAdmin = true
	if _, err := d.sql.ExecContext(ctx, `UPDATE users SET is_admin=1 WHERE id=?`, admin.ID); err != nil {
		t.Fatal(err)
	}
	cover := "new"
	if err := d.UpdatePlaylistPresentationChecked(ctx, "bob-pl", &admin, "保留", &cover); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.ExecContext(ctx, `DELETE FROM users WHERE id=?`, alice.ID); err != nil {
		t.Fatal(err)
	}
	p, _ := d.GetPlaylist(ctx, "bob-pl")
	if p.CoverID != "" {
		t.Fatal("删除上传者留下失效封面引用")
	}
}
