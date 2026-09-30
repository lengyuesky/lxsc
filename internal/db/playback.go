package db

import (
	"context"
	"encoding/json"
	"errors"
)

type PlayQueue struct {
	IDs       []string `json:"ids"`
	Current   string   `json:"current"`
	Position  int64    `json:"position"`
	ChangedAt int64    `json:"changedAt"`
	ChangedBy string   `json:"changedBy"`
}

func (d *DB) SavePlayQueue(ctx context.Context, userID int64, q PlayQueue) error {
	if len(q.IDs) > MaxPlaylistTracks || q.Position < 0 {
		return errors.New("播放队列参数无效")
	}
	raw, err := json.Marshal(q.IDs)
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `INSERT INTO play_queues(user_id,ids,current_id,position,changed_at,changed_by) VALUES(?,?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET ids=excluded.ids,current_id=excluded.current_id,position=excluded.position,changed_at=excluded.changed_at,changed_by=excluded.changed_by`, userID, string(raw), q.Current, q.Position, now(), q.ChangedBy)
	return err
}
func (d *DB) GetPlayQueue(ctx context.Context, userID int64) (*PlayQueue, error) {
	q := &PlayQueue{}
	var raw string
	err := d.sql.QueryRowContext(ctx, `SELECT ids,current_id,position,changed_at,changed_by FROM play_queues WHERE user_id=?`, userID).Scan(&raw, &q.Current, &q.Position, &q.ChangedAt, &q.ChangedBy)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal([]byte(raw), &q.IDs)
	return q, err
}
func (d *DB) SetRating(ctx context.Context, userID int64, id string, rating int) error {
	if rating < 0 || rating > 5 {
		return errors.New("评分必须为0–5")
	}
	if rating == 0 {
		_, err := d.sql.ExecContext(ctx, `DELETE FROM ratings WHERE user_id=? AND item_id=?`, userID, id)
		return err
	}
	_, err := d.sql.ExecContext(ctx, `INSERT INTO ratings(user_id,item_id,rating) VALUES(?,?,?) ON CONFLICT(user_id,item_id) DO UPDATE SET rating=excluded.rating`, userID, id, rating)
	return err
}
func (d *DB) Ratings(ctx context.Context, userID int64) (map[string]int, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT item_id,rating FROM ratings WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var rating int
		if err = rows.Scan(&id, &rating); err != nil {
			return nil, err
		}
		out[id] = rating
	}
	return out, rows.Err()
}
func (d *DB) SetNowPlaying(ctx context.Context, userID int64, client, id string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM now_playing WHERE user_id=? AND updated_at<?`, userID, now()-600); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO now_playing(user_id,client,track_id,updated_at) VALUES(?,?,?,?) ON CONFLICT(user_id,client) DO UPDATE SET track_id=excluded.track_id,updated_at=excluded.updated_at`, userID, client, id, now()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM now_playing WHERE user_id=? AND client NOT IN(SELECT client FROM now_playing WHERE user_id=? ORDER BY updated_at DESC,client LIMIT 20)`, userID, userID); err != nil {
		return err
	}
	return tx.Commit()
}

type NowPlaying struct {
	TrackID, Client, Username string
	UpdatedAt                 int64
}

func (d *DB) NowPlaying(ctx context.Context, userID int64) ([]NowPlaying, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT n.track_id,n.client,u.name,n.updated_at FROM now_playing n JOIN users u ON u.id=n.user_id WHERE n.user_id=? AND n.updated_at>? ORDER BY n.updated_at DESC LIMIT 20`, userID, now()-600)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NowPlaying{}
	for rows.Next() {
		var n NowPlaying
		if err = rows.Scan(&n.TrackID, &n.Client, &n.Username, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
