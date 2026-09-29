package db

import "context"

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
