package db

import (
	"context"
	"database/sql"
)

// CanReadPlaylist 统一歌单所有者、管理员和公开访问的读取规则。
func CanReadPlaylist(user *User, playlist *Playlist) bool {
	return user != nil && playlist != nil && (user.IsAdmin || playlist.UserID == user.ID || playlist.Public)
}

// CanEditPlaylist 的公开歌单权限与私有歌单一致，只允许所有者及管理员修改。
func CanEditPlaylist(user *User, playlist *Playlist) bool {
	return user != nil && playlist != nil && (user.IsAdmin || playlist.UserID == user.ID)
}

// CanReadLibraryTrack 在交付时重新检查引用权限，不把客户端标识当作授权。
func (d *DB) CanReadLibraryTrack(ctx context.Context, user *User, trackID string) (bool, error) {
	return canReadLibraryTrack(ctx, d.read, user, trackID)
}

type trackPermissionReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func canReadLibraryTrack(ctx context.Context, reader trackPermissionReader, user *User, trackID string) (bool, error) {
	if user == nil {
		return false, nil
	}
	var visible bool
	err := reader.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM stars WHERE user_id=? AND kind='track' AND item_id=?) OR
		EXISTS(SELECT 1 FROM playlist_tracks t JOIN playlists p ON p.id=t.playlist_id
			WHERE t.track_id=? AND (p.user_id=? OR p.public=1 OR ?=1)) OR
		EXISTS(SELECT 1 FROM history WHERE user_id=? AND track_id=?)`,
		user.ID, trackID, trackID, user.ID, boolInt(user.IsAdmin), user.ID, trackID).Scan(&visible)
	return visible, err
}

// TrackWriteAuthority 在插入任何新引用前复查原始库权限及协议持有的临时授权。
// Check 只检查协议会话状态，不得在回调中使用同一个数据库连接。
type TrackWriteAuthority struct {
	Actor      *User
	LibraryIDs []string
	Check      func() error
}

func (a TrackWriteAuthority) validate(ctx context.Context, tx *sql.Tx) error {
	if a.Actor != nil {
		var current bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND password_enc=? AND is_admin=?)`, a.Actor.ID, a.Actor.PasswordEnc, boolInt(a.Actor.IsAdmin)).Scan(&current); err != nil {
			return err
		}
		if !current {
			return ErrNotFound
		}
	}
	if a.Check != nil {
		if err := a.Check(); err != nil {
			return err
		}
	}
	for _, id := range a.LibraryIDs {
		allowed, err := canReadLibraryTrack(ctx, tx, a.Actor, id)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrNotFound
		}
	}
	return nil
}

// LibraryTrackIDs 一次查询合并收藏、可见歌单和最近播放，避免按歌单逐一查询。
// 歌单可见范围保持为自己的歌单与其他用户的公开歌单。
func (d *DB) LibraryTrackIDs(ctx context.Context, userID int64) ([]string, error) {
	rows, err := d.read.QueryContext(ctx, `WITH recent AS (
		SELECT track_id FROM history WHERE user_id=? GROUP BY track_id
		ORDER BY MAX(played_at) DESC, track_id LIMIT 200
	)
	SELECT item_id AS track_id FROM stars WHERE user_id=? AND kind='track'
	UNION SELECT t.track_id FROM playlist_tracks t JOIN playlists p ON p.id=t.playlist_id
		WHERE p.user_id=? OR p.public=1
	UNION SELECT track_id FROM recent
	ORDER BY track_id`, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
