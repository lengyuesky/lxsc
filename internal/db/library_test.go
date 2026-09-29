package db

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLibraryVisibilityDeduplicationAndHistory(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	a, err := d.CreateUser(ctx, "甲", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.CreateUser(ctx, "乙", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		id     string
		owner  int64
		public bool
		tracks []string
	}{{"own", a.ID, false, []string{"tr-wy-own", "tr-wy-own"}}, {"public", b.ID, true, []string{"tr-wy-public", "tr-wy-own"}}, {"private", b.ID, false, []string{"tr-wy-private"}}} {
		if err = d.CreatePlaylistFull(ctx, p.id, p.owner, p.id, "", p.public, p.tracks); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.Star(ctx, a.ID, "tr-wy-star", "track"); err != nil {
		t.Fatal(err)
	}
	if err = d.Star(ctx, b.ID, "tr-wy-secret", "track"); err != nil {
		t.Fatal(err)
	}
	if err = d.Star(ctx, a.ID, "al-wy-album", "album"); err != nil {
		t.Fatal(err)
	}
	if err = d.AddHistory(ctx, a.ID, "tr-wy-history", 1); err != nil {
		t.Fatal(err)
	}
	if err = d.AddHistory(ctx, a.ID, "tr-wy-own", 2); err != nil {
		t.Fatal(err)
	}
	got, err := d.LibraryTrackIDs(ctx, a.ID)
	want := []string{"tr-wy-history", "tr-wy-own", "tr-wy-public", "tr-wy-star"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("可见范围或去重异常：%v，%v", got, err)
	}
	public := false
	if err = d.UpdatePlaylistMeta(ctx, "public", nil, nil, &public); err != nil {
		t.Fatal(err)
	}
	got, err = d.LibraryTrackIDs(ctx, a.ID)
	if err != nil || len(got) != 3 {
		t.Fatal("公开权限变化未及时生效", got, err)
	}
	for i := 0; i < 220; i++ {
		if err = d.AddHistory(ctx, a.ID, fmt.Sprintf("tr-wy-recent-%03d", i), int64(i+100)); err != nil {
			t.Fatal(err)
		}
	}
	got, err = d.LibraryTrackIDs(ctx, a.ID)
	if err != nil || len(got) != 202 {
		t.Fatal("历史应限制为最近200首再合并收藏和歌单", len(got), err)
	}
}
