package db

import "context"

// CanReadPlaylist 统一歌单所有者、管理员和公开访问的读取规则。
func CanReadPlaylist(user *User, playlist *Playlist) bool {
	return user != nil && playlist != nil && (user.IsAdmin || playlist.UserID == user.ID || playlist.Public)
}

// CanReadLibraryTrack 在交付时重新检查引用权限，不把客户端标识当作授权。
func (d *DB) CanReadLibraryTrack(ctx context.Context, user *User, trackID string) (bool, error) {
	if user == nil {
		return false, nil
	}
	var visible bool
	err := d.read.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM stars WHERE user_id=? AND kind='track' AND item_id=?) OR
		EXISTS(SELECT 1 FROM playlist_tracks t JOIN playlists p ON p.id=t.playlist_id
			WHERE t.track_id=? AND (p.user_id=? OR p.public=1 OR ?=1)) OR
		EXISTS(SELECT 1 FROM history WHERE user_id=? AND track_id=?)`,
		user.ID, trackID, trackID, user.ID, boolInt(user.IsAdmin), user.ID, trackID).Scan(&visible)
	return visible, err
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
