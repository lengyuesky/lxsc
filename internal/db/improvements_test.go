package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyKeyMigrationAndLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`INSERT INTO users(id,name,password_enc,created_at) VALUES(1,'legacy','enc',1); INSERT INTO api_keys(key,user_id,label,created_at) VALUES('old-secret',1,'phone',1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	u, err := d.GetUserByAPIKey(ctx, "old-secret")
	if err != nil || u.Name != "legacy" {
		t.Fatalf("legacy auth: %v %v", u, err)
	}
	var stored string
	if err = d.sql.QueryRow(`SELECT key FROM api_keys`).Scan(&stored); err != nil || stored == "old-secret" {
		t.Fatalf("credential not migrated: %v", err)
	}
	if _, err = d.GetUserByAPIKey(ctx, stored); err == nil {
		t.Fatal("stored digest must not authenticate")
	}
	if err = d.CreateExpiringAPIKey(ctx, 1, "expired", "expired", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	if _, err = d.GetUserByAPIKey(ctx, "expired"); err == nil {
		t.Fatal("expired key accepted")
	}
	keys, err := d.ListAPIKeys(ctx, 1)
	if err != nil || len(keys) != 2 {
		t.Fatalf("keys: %v %v", keys, err)
	}
	if err = d.RevokeAPIKey(ctx, 2, keyDigest("old-secret")); err == nil {
		t.Fatal("cross-user revoke allowed")
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err = d.GetUserByAPIKey(ctx, "old-secret"); err != nil {
		t.Fatal("repeat migration hashed key twice", err)
	}
	if err = d.RevokeAPIKey(ctx, 1, keyDigest("old-secret")); err != nil {
		t.Fatal(err)
	}
	if _, err = d.GetUserByAPIKey(ctx, "old-secret"); err == nil {
		t.Fatal("revoked key accepted")
	}
}

func TestPlaybackStateIsolationAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "playback.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := d.CreateUser(ctx, "a", "enc", false, "320k")
	b, _ := d.CreateUser(ctx, "b", "enc", false, "320k")
	q := PlayQueue{IDs: []string{"tr-wy-1", "tr-tx-2", "tr-wy-1"}, Current: "tr-wy-1", Position: 1234, ChangedBy: "phone"}
	if err = d.SavePlayQueue(ctx, a.ID, q); err != nil {
		t.Fatal(err)
	}
	if err = d.SetRating(ctx, a.ID, "tr-wy-1", 5); err != nil {
		t.Fatal(err)
	}
	if _, err = d.GetPlayQueue(ctx, b.ID); err == nil {
		t.Fatal("queue leaked")
	}
	ratings, err := d.Ratings(ctx, b.ID)
	if err != nil || len(ratings) != 0 {
		t.Fatal("ratings leaked", err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	saved, err := d.GetPlayQueue(ctx, a.ID)
	if err != nil || saved.Position != 1234 || len(saved.IDs) != 3 || saved.IDs[0] != saved.IDs[2] {
		t.Fatalf("queue: %+v %v", saved, err)
	}
	if err = d.SetRating(ctx, a.ID, "tr-wy-1", 0); err != nil {
		t.Fatal(err)
	}
	ratings, _ = d.Ratings(ctx, a.ID)
	if len(ratings) != 0 {
		t.Fatal("rating not cleared")
	}
}

func TestSubscriptionPermissionAndConcurrentEdits(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "sub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, _ := d.CreateUser(ctx, "a", "enc", false, "320k")
	b, _ := d.CreateUser(ctx, "b", "enc", false, "320k")
	if err = d.CreatePlaylistFull(ctx, "pl-a", a.ID, "test", "", false, []string{"tr-wy-1"}); err != nil {
		t.Fatal(err)
	}
	if err = d.SaveSubscription(ctx, "pl-a", b, "wy", "123", true); err == nil {
		t.Fatal("other user subscribed")
	}
	if err = d.SaveSubscription(ctx, "pl-a", a, "wy", "123", true); err != nil {
		t.Fatal(err)
	}
	sub, err := d.Subscription(ctx, "pl-a")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.SaveSubscription(ctx, "pl-a", a, "tx", "456", false); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ApplySubscription(ctx, sub, a, TracksRevision([]string{"tr-wy-1"}), []string{"tr-wy-2"}, nil, nil); err == nil {
		t.Fatal("stale source overwrote playlist")
	}
	sub, _ = d.Subscription(ctx, "pl-a")
	if _, err = d.ApplySubscription(ctx, sub, a, "stale", nil, nil, nil); err == nil {
		t.Fatal("stale playlist overwritten")
	}
	p, err := d.ApplySubscription(ctx, sub, a, TracksRevision([]string{"tr-wy-1"}), []string{"tr-wy-1", "tr-tx-2"}, nil, []string{"tr-tx-2"})
	if err != nil || len(p.TrackIDs) != 2 {
		t.Fatalf("apply: %v %v", p, err)
	}
}

func TestHistoryRetentionPreservesStatisticsAndLimitsBatch(t *testing.T) {
	ctx := context.Background()
	d, err := Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, _ := d.CreateUser(ctx, "a", "enc", false, "320k")
	if _, err = d.sql.Exec(`WITH RECURSIVE n(x) AS(VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10005) INSERT INTO history(user_id,track_id,played_at) SELECT ?,'tr-wy-1',1 FROM n`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.sql.Exec(`INSERT INTO listening_sessions(user_id,session_key,source,track_id,name,singer,started_at) VALUES(?,'test','web','tr-wy-1','test','artist',?)`, u.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = d.sql.Exec(`INSERT INTO listening_days(session_id,day,milliseconds) SELECT id,?,30000 FROM listening_sessions`, time.Now().In(ListeningZone).Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	n, err := d.HistoryRetention(ctx, 365, false)
	if err != nil || n != 10005 {
		t.Fatalf("preview %d %v", n, err)
	}
	n, err = d.HistoryRetention(ctx, 365, true)
	if err != nil || n != 10000 {
		t.Fatalf("batch %d %v", n, err)
	}
	var ms int64
	if err = d.sql.QueryRow(`SELECT SUM(milliseconds) FROM listening_days`).Scan(&ms); err != nil || ms != 30000 {
		t.Fatal("statistics changed", ms, err)
	}
	ids, err := d.SmartTrackIDs(ctx, u.ID, "frequent", "")
	if err != nil || len(ids) != 1 || ids[0] != "tr-wy-1" {
		t.Fatalf("web listening discovery missing: %v %v", ids, err)
	}
	ids, err = d.SmartTrackIDs(ctx, u.ID+1, "frequent", "")
	if err != nil || len(ids) != 0 {
		t.Fatal("discovery leaked", ids, err)
	}
}
