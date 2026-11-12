package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrCoverLimit = errors.New("未使用的封面数量超限")

func ownedCoverTx(ctx context.Context, tx *sql.Tx, userID int64, id string) error {
	var ownerID int64
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM playlist_covers WHERE id=?`, id).Scan(&ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if ownerID != userID {
		return ErrPlaylistForbidden
	}
	return nil
}

// SavePlaylistCover 保存经过验证的图片；清理未绑定且超过一天的上传，并限制待使用图片数。
func (d *DB) SavePlaylistCover(ctx context.Context, id string, userID int64, contentType string, data []byte) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_covers WHERE created_at<? AND NOT EXISTS(SELECT 1 FROM playlists WHERE cover_id=playlist_covers.id)`, time.Now().Add(-24*time.Hour).Unix()); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM playlist_covers c WHERE user_id=? AND NOT EXISTS(SELECT 1 FROM playlists WHERE cover_id=c.id)`, userID).Scan(&count); err != nil {
		return err
	}
	if count >= 10 {
		return ErrCoverLimit
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO playlist_covers(id,user_id,content_type,data,created_at) VALUES(?,?,?,?,?)`, id, userID, contentType, data, now()); err != nil {
		return err
	}
	return tx.Commit()
}

// ReadPlaylistCover 在读取时检查上传者或当前可读歌单的权限，公开权限撤销立即生效。
func (d *DB) ReadPlaylistCover(ctx context.Context, id string, actor *User) ([]byte, string, error) {
	if actor == nil {
		return nil, "", ErrNotFound
	}
	var data []byte
	var contentType string
	err := d.read.QueryRowContext(ctx, `SELECT c.data,c.content_type FROM playlist_covers c WHERE c.id=? AND (
		c.user_id=? OR EXISTS(SELECT 1 FROM playlists p WHERE p.cover_id=c.id AND (p.user_id=? OR p.public=1 OR ?=1)))`, id, actor.ID, actor.ID, boolInt(actor.IsAdmin)).Scan(&data, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return data, contentType, err
}
