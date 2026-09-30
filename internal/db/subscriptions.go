package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

type Subscription struct {
	PlaylistID string   `json:"playlistId"`
	Source     string   `json:"source"`
	RemoteID   string   `json:"remoteId"`
	Auto       bool     `json:"auto"`
	Version    int64    `json:"version"`
	Seen       []string `json:"-"`
	NextAt     int64    `json:"nextAt"`
	LastAt     int64    `json:"lastAt"`
	Error      string   `json:"error"`
}

func (d *DB) Subscription(ctx context.Context, id string) (*Subscription, error) {
	var s Subscription
	var raw string
	err := d.sql.QueryRowContext(ctx, `SELECT playlist_id,source,remote_id,auto,version,seen,next_at,last_at,error FROM playlist_subscriptions WHERE playlist_id=?`, id).Scan(&s.PlaylistID, &s.Source, &s.RemoteID, &s.Auto, &s.Version, &raw, &s.NextAt, &s.LastAt, &s.Error)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(raw), &s.Seen)
	return &s, err
}
func (d *DB) SaveSubscription(ctx context.Context, id string, actor *User, source, remote string, auto bool) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = editablePlaylistTx(ctx, tx, id, actor); err != nil {
		return err
	}
	if source == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM playlist_subscriptions WHERE playlist_id=?`, id)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO playlist_subscriptions(playlist_id,source,remote_id,auto,version,next_at) VALUES(?,?,?,?,?,?) ON CONFLICT(playlist_id) DO UPDATE SET seen=CASE WHEN source=excluded.source AND remote_id=excluded.remote_id THEN seen ELSE '[]' END,source=excluded.source,remote_id=excluded.remote_id,auto=excluded.auto,version=MAX(version+1,excluded.version),next_at=excluded.next_at,error=''`, id, source, remote, auto, timeVersion(), now()+86400)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) ApplySubscription(ctx context.Context, s *Subscription, actor *User, revision string, ids []string, tracks []Track, seen []string) (*Playlist, error) {
	if len(ids) > MaxPlaylistTracks {
		return nil, ErrPlaylistLimit
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	p, err := editablePlaylistTx(ctx, tx, s.PlaylistID, actor)
	if err != nil {
		return nil, err
	}
	if TracksRevision(p.TrackIDs) != revision {
		return nil, ErrPlaylistConflict
	}
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT version FROM playlist_subscriptions WHERE playlist_id=?`, s.PlaylistID).Scan(&version); err != nil {
		return nil, err
	}
	if version != s.Version {
		return nil, ErrPlaylistConflict
	}
	if err = upsertTracksTx(ctx, tx, tracks); err != nil {
		return nil, err
	}
	if err = replacePlaylistTracksTx(ctx, tx, p.ID, ids); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(seen)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE playlist_subscriptions SET seen=?,next_at=?,last_at=?,error='',version=? WHERE playlist_id=?`, string(raw), now()+86400, now(), timeVersion(), p.ID); err != nil {
		return nil, err
	}
	p, err = editablePlaylistTx(ctx, tx, p.ID, actor)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}
func (d *DB) DueSubscriptions(ctx context.Context) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT playlist_id FROM playlist_subscriptions WHERE auto=1 AND next_at<=? ORDER BY next_at LIMIT 10`, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (d *DB) SubscriptionFailed(ctx context.Context, s *Subscription) error {
	if s == nil {
		return errors.New("subscription missing")
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE playlist_subscriptions SET next_at=?,error=? WHERE playlist_id=? AND version=?`, now()+3600, "更新失败，将于一小时后重试；请检查平台与歌单状态", s.PlaylistID, s.Version)
	return err
}

func timeVersion() int64 { return time.Now().UnixMicro() }
