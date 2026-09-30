package portal

import (
	"lxsc/internal/db"
	"lxsc/internal/music"
	"testing"
)

func TestSubscriptionPreservesLocalEdits(t *testing.T) {
	track := func(id string) *music.Info {
		return music.FromMap(map[string]any{"source": "wy", "songmid": id, "name": "test"})
	}
	one, two, three := track("1"), track("2"), track("3")
	p := &db.Playlist{TrackIDs: []string{three.TrackID(), "tr-tx-local"}}
	sub := &db.Subscription{Seen: []string{one.TrackID(), three.TrackID()}}
	remote := &music.OnlinePlaylist{Tracks: []*music.Info{one, two}}
	ids, rows, seen, removed := subscriptionChanges(p, sub, remote)
	if len(ids) != 3 || ids[0] != three.TrackID() || ids[1] != "tr-tx-local" || ids[2] != two.TrackID() || len(rows) != 1 || len(seen) != 2 || removed != 1 {
		t.Fatalf("local edits lost: %v %v %v %d", ids, rows, seen, removed)
	}
}
