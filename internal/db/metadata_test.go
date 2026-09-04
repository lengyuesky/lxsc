package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestCleanupUnreferencedMetadata(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.CreateUser(ctx, "owner", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	tracks := []Track{
		{ID: "tr-wy-playlist", Source: "wy", Name: "歌单歌曲", JSON: []byte(`{"source":"wy","songmid":"playlist"}`)},
		{ID: "tr-wy-star", Source: "wy", Name: "收藏歌曲", JSON: []byte(`{"source":"wy","songmid":"star"}`)},
		{ID: "tr-wy-history", Source: "wy", Name: "历史歌曲", JSON: []byte(`{"source":"wy","songmid":"history"}`)},
		{ID: "tr-wy-album", Source: "wy", Name: "收藏专辑歌曲", JSON: []byte(`{"source":"wy","songmid":"album"}`)},
		{ID: "tr-wy-temp", Source: "wy", Name: "临时歌曲", JSON: []byte(`{"source":"wy","songmid":"temp"}`)},
	}
	if err := database.UpsertTracks(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	if err := database.CreatePlaylistFull(ctx, "pl-test", user.ID, "歌单", "", false, []string{"tr-wy-playlist"}); err != nil {
		t.Fatal(err)
	}
	if err := database.Star(ctx, user.ID, "tr-wy-star", "track"); err != nil {
		t.Fatal(err)
	}
	if err := database.AddHistory(ctx, user.ID, "tr-wy-history", 1); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertAlbum(ctx, "al-wy-1", "wy", "收藏专辑", "歌手", []byte(`["tr-wy-album"]`)); err != nil {
		t.Fatal(err)
	}
	if err := database.Star(ctx, user.ID, "al-wy-1", "album"); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertAlbum(ctx, "al-wy-temp", "wy", "临时专辑", "歌手", []byte(`["tr-wy-temp"]`)); err != nil {
		t.Fatal(err)
	}
	if err := database.UpsertArtist(ctx, "ar-temp", "wy", "临时歌手", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}

	result, err := database.CleanupUnreferencedMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tracks != 1 || result.Albums != 1 || result.Artists != 1 {
		t.Fatalf("清理数量异常: %+v", result)
	}
	for _, id := range []string{"tr-wy-playlist", "tr-wy-star", "tr-wy-history", "tr-wy-album"} {
		if _, err := database.GetTrack(ctx, id); err != nil {
			t.Fatalf("被引用歌曲不应删除 %s: %v", id, err)
		}
	}
	if _, err := database.GetTrack(ctx, "tr-wy-temp"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("临时歌曲应被删除: %v", err)
	}
}
