package music

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/settings"
)

func TestAlbumAndArtistCachesStayInMemory(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := NewCatalog(database, nil, nil, store, log)
	one := FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "一", "singer": "歌手", "albumId": "album"})
	two := FromMap(map[string]any{"source": "wy", "songmid": "two", "name": "二", "singer": "歌手", "albumId": "album"})

	catalog.CacheAlbum(one.AlbumSubID(), []*Info{one})
	catalog.CacheAlbum(one.AlbumSubID(), []*Info{two})
	album, ok := catalog.CachedAlbum(one.AlbumSubID())
	if !ok || len(album) != 2 {
		t.Fatalf("专辑内存缓存应合并歌曲: ok=%v len=%d", ok, len(album))
	}
	catalog.CacheArtist(one.ArtistSubID(), "歌手")
	if name, ok := catalog.CachedArtist(one.ArtistSubID()); !ok || name != "歌手" {
		t.Fatalf("歌手内存缓存异常: %q %v", name, ok)
	}
	if _, _, _, _, err := database.GetAlbum(ctx, one.AlbumSubID()); err == nil {
		t.Fatal("临时专辑不应写入数据库")
	}
	if _, _, _, err := database.GetArtist(ctx, one.ArtistSubID()); err == nil {
		t.Fatal("临时歌手不应写入数据库")
	}
}

func TestCacheDoesNotPersistTransientTracks(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := NewCatalog(database, nil, nil, store, log)
	info := FromMap(map[string]any{"source": "wy", "songmid": "temporary", "name": "临时歌曲"})

	catalog.Cache([]*Info{info})
	if _, err := database.GetTrack(ctx, info.TrackID()); err == nil {
		t.Fatal("临时缓存不应写入 tracks 表")
	}
	if cached, ok := catalog.tracks.Get(info.TrackID()); !ok || cached.Name() != "临时歌曲" {
		t.Fatal("临时歌曲应保留在内存缓存中")
	}
	if err := catalog.RememberSync(ctx, []*Info{info}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetTrack(ctx, info.TrackID()); err != nil {
		t.Fatalf("长期保留的歌曲应写入 tracks 表: %v", err)
	}
}
