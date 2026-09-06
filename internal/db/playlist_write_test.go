package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func newPlaylistWriteDB(t *testing.T) (*DB, *User, *User) {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	alice, err := d.CreateUser(context.Background(), "alice", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := d.CreateUser(context.Background(), "bob", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	return d, alice, bob
}

func testPlaylistTrack(id string) Track {
	return Track{ID: id, Source: "wy", Name: "同名歌曲", JSON: []byte(`{"source":"wy","songmid":"测试","name":"同名歌曲"}`)}
}

func TestTracksRevision(t *testing.T) {
	if TracksRevision(nil) != TracksRevision([]string{}) {
		t.Fatal("空列表版本必须稳定")
	}
	seen := map[string]bool{}
	for _, ids := range [][]string{nil, {"a"}, {"a", "a"}, {"a", "b"}, {"b", "a"}, {"a,b"}} {
		revision := TracksRevision(ids)
		if seen[revision] || len(revision) != 64 {
			t.Fatalf("版本应区分顺序、重复项与分隔字符: %v", ids)
		}
		seen[revision] = true
	}
}

func TestPrependPlaylistTrackOrderingAndPermissions(t *testing.T) {
	ctx := context.Background()
	d, alice, bob := newPlaylistWriteDB(t)
	if err := d.CreatePlaylistFull(ctx, "pl-test", alice.ID, "测试", "", true, []string{"tr-wy-old", "tr-wy-old"}); err != nil {
		t.Fatal(err)
	}
	added, err := d.PrependPlaylistTrack(ctx, "pl-test", alice, testPlaylistTrack("tr-wy-new"))
	if err != nil || !added {
		t.Fatalf("置顶收藏失败: %v %v", added, err)
	}
	p, _ := d.GetPlaylist(ctx, "pl-test")
	if !reflect.DeepEqual(p.TrackIDs, []string{"tr-wy-new", "tr-wy-old", "tr-wy-old"}) {
		t.Fatalf("新增应置顶且不移除旧重复项: %v", p.TrackIDs)
	}
	if _, err := d.sql.ExecContext(ctx, `UPDATE playlists SET updated_at=1 WHERE id='pl-test'`); err != nil {
		t.Fatal(err)
	}
	added, err = d.PrependPlaylistTrack(ctx, "pl-test", alice, testPlaylistTrack("tr-wy-old"))
	p, _ = d.GetPlaylist(ctx, "pl-test")
	if err != nil || added || p.UpdatedAt != 1 || p.Count != 3 || p.TrackIDs[0] != "tr-wy-new" {
		t.Fatalf("重复收藏不应重排或更新时间: %+v added=%v err=%v", p, added, err)
	}
	if _, err := d.GetTrack(ctx, "tr-wy-old"); err != nil {
		t.Fatal("重复收藏仍应修复历史缺失元数据")
	}
	if _, err := d.PrependPlaylistTrack(ctx, "pl-test", bob, testPlaylistTrack("tr-wy-forbidden")); !errors.Is(err, ErrPlaylistForbidden) {
		t.Fatalf("公开不代表可编辑: %v", err)
	}
	if _, err := d.GetTrack(ctx, "tr-wy-forbidden"); !errors.Is(err, ErrNotFound) {
		t.Fatal("拒绝请求不应保存元数据")
	}
	admin := *bob
	admin.IsAdmin = true
	if added, err := d.PrependPlaylistTrack(ctx, "pl-test", &admin, testPlaylistTrack("tr-tx-new")); err != nil || !added {
		t.Fatalf("管理员可收藏到他人歌单，跨平台同名保留: %v", err)
	}
	if _, err := d.PrependPlaylistTrack(ctx, "pl-missing", alice, testPlaylistTrack("tr-wy-new")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("已删除目标应失败: %v", err)
	}
}

func TestConcurrentCollectionsAndDraftRevision(t *testing.T) {
	ctx := context.Background()
	d, alice, _ := newPlaylistWriteDB(t)
	if err := d.CreatePlaylistFull(ctx, "pl-test", alice.ID, "并发", "", false, []string{"tr-wy-old"}); err != nil {
		t.Fatal(err)
	}
	baseline := TracksRevision([]string{"tr-wy-old"})
	var wg sync.WaitGroup
	failures := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := d.PrependPlaylistTrack(ctx, "pl-test", alice, testPlaylistTrack(fmt.Sprintf("tr-wy-%d", i%20)))
			if err != nil {
				failures <- err
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	p, _ := d.GetPlaylist(ctx, "pl-test")
	seen := map[string]bool{}
	for _, id := range p.TrackIDs {
		if seen[id] {
			t.Fatalf("并发重复收藏插入了重复歌曲: %v", p.TrackIDs)
		}
		seen[id] = true
	}
	if p.Count != 21 || p.TrackIDs[20] != "tr-wy-old" {
		t.Fatalf("并发收藏丢歌或破坏原有顺序: %v", p.TrackIDs)
	}
	rows := []Track{testPlaylistTrack("tr-wy-conflict")}
	_, err := d.ReplacePlaylistTracksChecked(ctx, "pl-test", alice, []string{"tr-wy-conflict"}, rows, &baseline)
	if !errors.Is(err, ErrPlaylistConflict) {
		t.Fatalf("旧草稿必须冲突: %v", err)
	}
	if _, err := d.GetTrack(ctx, rows[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("版本冲突不应写入元数据")
	}
	revision := TracksRevision(p.TrackIDs)
	ids := []string{"tr-wy-new", "tr-wy-new"}
	receipt, err := d.ReplacePlaylistTracksChecked(ctx, "pl-test", alice, ids, []Track{testPlaylistTrack(ids[0])}, &revision)
	if err != nil {
		t.Fatal(err)
	}
	p, _ = d.GetPlaylist(ctx, "pl-test")
	if !reflect.DeepEqual(p.TrackIDs, ids) {
		t.Fatal("完整替换必须继续允许重复")
	}
	if _, err := d.PrependPlaylistTrack(ctx, "pl-test", alice, testPlaylistTrack("tr-wy-after-commit")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(receipt.TrackIDs, ids) || receipt.Count != len(ids) {
		t.Fatalf("提交回执不能采用后续收藏的列表或版本: %+v", receipt)
	}
	confirmed := TracksRevision(receipt.TrackIDs)
	if _, err := d.ReplacePlaylistTracksChecked(ctx, "pl-test", alice, ids, nil, &confirmed); !errors.Is(err, ErrPlaylistConflict) {
		t.Fatalf("依据本次提交回执继续编辑时必须发现后来的收藏: %v", err)
	}
}

func TestCollectionCapacity(t *testing.T) {
	ctx := context.Background()
	d, alice, _ := newPlaylistWriteDB(t)
	ids := make([]string, MaxPlaylistTracks)
	for i := range ids {
		ids[i] = fmt.Sprintf("tr-wy-%d", i)
	}
	if err := d.CreatePlaylistFull(ctx, "pl-full", alice.ID, "满", "", false, ids); err != nil {
		t.Fatal(err)
	}
	if added, err := d.PrependPlaylistTrack(ctx, "pl-full", alice, testPlaylistTrack(ids[0])); err != nil || added {
		t.Fatalf("满歌单重复收藏应成功无操作: %v %v", added, err)
	}
	if _, err := d.PrependPlaylistTrack(ctx, "pl-full", alice, testPlaylistTrack("tr-wy-extra")); !errors.Is(err, ErrPlaylistLimit) {
		t.Fatalf("超限应拒绝: %v", err)
	}
	if _, err := d.GetTrack(ctx, "tr-wy-extra"); !errors.Is(err, ErrNotFound) {
		t.Fatal("超限不能留下元数据")
	}
}

func TestPlaylistMetadataTransactionsRollback(t *testing.T) {
	ctx := context.Background()
	d, alice, _ := newPlaylistWriteDB(t)
	if err := d.CreatePlaylistFull(ctx, "pl-test", alice.ID, "原有", "", false, []string{"tr-wy-old"}); err != nil {
		t.Fatal(err)
	}
	_, err := d.sql.ExecContext(ctx, `CREATE TRIGGER reject_test_track BEFORE INSERT ON playlist_tracks WHEN NEW.track_id='tr-wy-fail' BEGIN SELECT RAISE(ABORT, '测试回滚'); END`)
	if err != nil {
		t.Fatal(err)
	}
	track := testPlaylistTrack("tr-wy-fail")
	if _, err := d.PrependPlaylistTrack(ctx, "pl-test", alice, track); err == nil {
		t.Fatal("模拟关系写入失败应返回错误")
	}
	if err := d.CreatePlaylistWithMetadata(ctx, "pl-new", alice.ID, "新建", "", false, []string{track.ID}, []Track{track}); err == nil {
		t.Fatal("新建并收藏应整体回滚")
	}
	if _, err := d.ReplacePlaylistTracksChecked(ctx, "pl-test", alice, []string{track.ID}, []Track{track}, nil); err == nil {
		t.Fatal("完整替换应整体回滚")
	}
	if _, err := d.GetTrack(ctx, track.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("失败事务不能留下元数据")
	}
	if _, err := d.GetPlaylist(ctx, "pl-new"); !errors.Is(err, ErrNotFound) {
		t.Fatal("失败创建不能留下空歌单")
	}
	p, _ := d.GetPlaylist(ctx, "pl-test")
	if !reflect.DeepEqual(p.TrackIDs, []string{"tr-wy-old"}) {
		t.Fatalf("失败替换应保留原有列表: %v", p.TrackIDs)
	}
}
