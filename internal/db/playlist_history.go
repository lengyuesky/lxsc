package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// 只保留最近十次成功保存前的歌曲列表，失败或冲突不产生记录。
func savePlaylistHistoryTx(ctx context.Context, tx *sql.Tx, playlistID string, ids []string) error {
	// 旧客户端可能保存超过网页上限的歌单；不阻止用户将它缩减到上限以内。
	if len(ids) > MaxPlaylistTracks {
		return nil
	}
	if ids == nil {
		ids = []string{}
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO playlist_track_history(playlist_id,created_at,ids) VALUES(?,?,?)`, playlistID, now(), string(raw)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM playlist_track_history WHERE playlist_id=? AND id NOT IN(SELECT id FROM playlist_track_history WHERE playlist_id=? ORDER BY id DESC LIMIT 10)`, playlistID, playlistID)
	return err
}

type PlaylistHistory struct {
	ID        int64 `json:"id"`
	CreatedAt int64 `json:"createdAt"`
	Count     int   `json:"count"`
}

func (d *DB) PlaylistHistory(ctx context.Context, playlistID string, actor *User) ([]PlaylistHistory, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = editablePlaylistTx(ctx, tx, playlistID, actor); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,created_at,json_array_length(ids) FROM playlist_track_history WHERE playlist_id=? ORDER BY id DESC LIMIT 10`, playlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlaylistHistory{}
	for rows.Next() {
		var h PlaylistHistory
		if err = rows.Scan(&h.ID, &h.CreatedAt, &h.Count); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (d *DB) PlaylistHistoryTracks(ctx context.Context, playlistID string, historyID int64, actor *User) ([]string, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = editablePlaylistTx(ctx, tx, playlistID, actor); err != nil {
		return nil, err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT ids FROM playlist_track_history WHERE playlist_id=? AND id=?`, playlistID, historyID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var ids []string
	err = json.Unmarshal([]byte(raw), &ids)
	return ids, err
}
