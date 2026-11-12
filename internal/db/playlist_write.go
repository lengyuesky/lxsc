package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// MaxPlaylistTracks 是网页及飞牛增量写入的歌曲数量上限；旧 Subsonic 整体替换契约不变。
const MaxPlaylistTracks = 2000

// 每批最多 900 个参数，兼容 SQLite 较低的变量上限；顺序和重复项原样保存。
func insertPlaylistTracksTx(ctx context.Context, tx *sql.Tx, id string, ids []string) error {
	const batchSize = 300
	for start := 0; start < len(ids); start += batchSize {
		end := min(start+batchSize, len(ids))
		args := make([]any, 0, (end-start)*3)
		for i := start; i < end; i++ {
			args = append(args, id, i, ids[i])
		}
		values := strings.TrimSuffix(strings.Repeat("(?,?,?),", end-start), ",")
		if _, err := tx.ExecContext(ctx, `INSERT INTO playlist_tracks(playlist_id,position,track_id) VALUES `+values, args...); err != nil {
			return err
		}
	}
	return nil
}

var (
	ErrPlaylistForbidden = errors.New("无权修改该歌单")
	ErrPlaylistConflict  = errors.New("歌单歌曲已被其他页面更新，请刷新后重新编辑；当前草稿仍保留")
	ErrPlaylistLimit     = errors.New("单个歌单最多包含 2000 首歌曲")
)

// TracksRevision 按有序 ID 生成稳定版本，区分顺序与重复项，统一空切片表示。
func TracksRevision(ids []string) string {
	if ids == nil {
		ids = []string{}
	}
	data, _ := json.Marshal(ids)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func editablePlaylistTx(ctx context.Context, tx *sql.Tx, id string, actor *User) (*Playlist, error) {
	if err := (TrackWriteAuthority{Actor: actor}).validate(ctx, tx); err != nil {
		return nil, err
	}
	var playlist Playlist
	var public int
	if err := tx.QueryRowContext(ctx, `SELECT p.id, p.user_id, u.name, p.name, p.comment, p.public, p.created_at, p.updated_at, p.cover_id FROM playlists p JOIN users u ON u.id=p.user_id WHERE p.id = ?`, id).
		Scan(&playlist.ID, &playlist.UserID, &playlist.Owner, &playlist.Name, &playlist.Comment, &public, &playlist.CreatedAt, &playlist.UpdatedAt, &playlist.CoverID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	playlist.Public = public == 1
	if !CanEditPlaylist(actor, &playlist) {
		return nil, ErrPlaylistForbidden
	}
	rows, err := tx.QueryContext(ctx, `SELECT track_id FROM playlist_tracks WHERE playlist_id = ? ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var trackID string
		if err := rows.Scan(&trackID); err != nil {
			return nil, err
		}
		ids = append(ids, trackID)
	}
	playlist.TrackIDs = ids
	playlist.Count = len(ids)
	return &playlist, rows.Err()
}

// ReplacePlaylistTracksChecked 在写事务内检查权限和草稿版本，并原子保存歌曲与元数据。
// expectedRevision 为 nil 时保持旧接口的无条件替换行为。
func (d *DB) ReplacePlaylistTracksChecked(ctx context.Context, id string, actor *User, ids []string, tracks []Track, expectedRevision *string) (*Playlist, error) {
	if len(ids) > MaxPlaylistTracks {
		return nil, ErrPlaylistLimit
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := editablePlaylistTx(ctx, tx, id, actor)
	if err != nil {
		return nil, err
	}
	if expectedRevision != nil && *expectedRevision != TracksRevision(current.TrackIDs) {
		return nil, ErrPlaylistConflict
	}
	if !slices.Equal(current.TrackIDs, ids) {
		if err := savePlaylistHistoryTx(ctx, tx, id, current.TrackIDs); err != nil {
			return nil, err
		}
	}
	if err := upsertTracksTx(ctx, tx, tracks); err != nil {
		return nil, err
	}
	if err := replacePlaylistTracksTx(ctx, tx, id, ids); err != nil {
		return nil, err
	}
	// 回包必须是本次事务的快照，不能在提交后读到其他收藏产生的新版本。
	result, err := editablePlaylistTx(ctx, tx, id, actor)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// PrependPlaylistTrack 增量置顶收藏；事务内读取最新列表，重复 ID 不改动顺序或时间。
func (d *DB) PrependPlaylistTrack(ctx context.Context, id string, actor *User, track Track) (bool, error) {
	added, err := d.PrependPlaylistTracks(ctx, id, actor, []Track{track})
	return added != 0, err
}

// PrependPlaylistTracks 原子加入一批歌曲，按请求顺序置顶，已存在的 ID 不重排。
// 协议标识与元数据、歌单关系一同提交；并发增量操作均以事务内的最新列表为准。
func (d *DB) PrependPlaylistTracks(ctx context.Context, id string, actor *User, tracks []Track, clientIDs ...ClientID) (int, error) {
	return d.PrependPlaylistTracksAuthorized(ctx, id, actor, tracks, TrackWriteAuthority{}, clientIDs...)
}

func (d *DB) PrependPlaylistTracksAuthorized(ctx context.Context, id string, actor *User, tracks []Track, authority TrackWriteAuthority, clientIDs ...ClientID) (int, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	playlist, err := editablePlaylistTx(ctx, tx, id, actor)
	if err != nil {
		return 0, err
	}
	if err := authority.validate(ctx, tx); err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(playlist.TrackIDs)+len(tracks))
	for _, id := range playlist.TrackIDs {
		seen[id] = true
	}
	addedIDs := make([]string, 0, len(tracks))
	for _, track := range tracks {
		if !seen[track.ID] {
			addedIDs = append(addedIDs, track.ID)
			seen[track.ID] = true
		}
	}
	if len(addedIDs) > 0 && len(playlist.TrackIDs)+len(addedIDs) > MaxPlaylistTracks {
		return 0, ErrPlaylistLimit
	}
	// 即便条目已经存在，也补齐早期版本可能只保存在内存里的元数据。
	if err := upsertTracksTx(ctx, tx, tracks); err != nil {
		return 0, err
	}
	if err := ensureClientIDsTx(ctx, tx, clientIDs); err != nil {
		return 0, err
	}
	if len(addedIDs) > 0 {
		ids := append(addedIDs, playlist.TrackIDs...)
		if err := replacePlaylistTracksTx(ctx, tx, id, ids); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(addedIDs), nil
}

// RemovePlaylistTracks 在事务内删除所选 ID 的全部位置，保留其他条目的顺序。
func (d *DB) RemovePlaylistTracks(ctx context.Context, id string, actor *User, trackIDs []string) (int, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	playlist, err := editablePlaylistTx(ctx, tx, id, actor)
	if err != nil {
		return 0, err
	}
	remove := make(map[string]bool, len(trackIDs))
	for _, id := range trackIDs {
		remove[id] = true
	}
	remaining := make([]string, 0, len(playlist.TrackIDs))
	for _, id := range playlist.TrackIDs {
		if !remove[id] {
			remaining = append(remaining, id)
		}
	}
	removed := len(playlist.TrackIDs) - len(remaining)
	if removed != 0 {
		if err := savePlaylistHistoryTx(ctx, tx, id, playlist.TrackIDs); err != nil {
			return 0, err
		}
		if err := replacePlaylistTracksTx(ctx, tx, id, remaining); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}

// RenamePlaylistChecked 只修改名称，并在同一写事务内复查权限。
func (d *DB) RenamePlaylistChecked(ctx context.Context, id string, actor *User, name string) error {
	return d.UpdatePlaylistPresentationChecked(ctx, id, actor, name, nil)
}

// UpdatePlaylistPresentationChecked 在一个事务内修改名称和可选封面，保留备注及公开状态。
func (d *DB) UpdatePlaylistPresentationChecked(ctx context.Context, id string, actor *User, name string, coverID *string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	playlist, err := editablePlaylistTx(ctx, tx, id, actor)
	if err != nil {
		return err
	}
	cover := playlist.CoverID
	if coverID != nil && *coverID != cover {
		if *coverID != "" {
			if err := ownedCoverTx(ctx, tx, actor.ID, *coverID); err != nil {
				return err
			}
		}
		cover = *coverID
	}
	if playlist.Name != name || cover != playlist.CoverID {
		if _, err := tx.ExecContext(ctx, `UPDATE playlists SET name=?,cover_id=?,updated_at=? WHERE id=?`, name, cover, now(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeletePlaylistChecked 在删除事务内复查权限，不根据先前读到的歌单授权。
func (d *DB) DeletePlaylistChecked(ctx context.Context, id string, actor *User) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := editablePlaylistTx(ctx, tx, id, actor); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlists WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
