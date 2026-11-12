package db

import "context"

// StarTracksWithMetadata 原子保存个人歌曲收藏、元数据及协议标识。
// 重复收藏保留原时间；任何一项失败都不产生部分收藏或无引用元数据。
func (d *DB) StarTracksWithMetadata(ctx context.Context, userID int64, tracks []Track, clientIDs ...ClientID) error {
	return d.StarTracksAuthorized(ctx, userID, tracks, TrackWriteAuthority{}, clientIDs...)
}

func (d *DB) StarTracksAuthorized(ctx context.Context, userID int64, tracks []Track, authority TrackWriteAuthority, clientIDs ...ClientID) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := authority.validate(ctx, tx); err != nil {
		return err
	}
	if err := upsertTracksTx(ctx, tx, tracks); err != nil {
		return err
	}
	if err := ensureClientIDsTx(ctx, tx, clientIDs); err != nil {
		return err
	}
	for _, track := range tracks {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO stars(user_id,item_id,kind,created_at) VALUES(?,?,'track',?)`, userID, track.ID, now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnstarTracks 只取消当前用户的歌曲收藏，不改变歌单或其他类型的收藏。
func (d *DB) UnstarTracks(ctx context.Context, userID int64, trackIDs []string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range trackIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM stars WHERE user_id=? AND item_id=? AND kind='track'`, userID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
