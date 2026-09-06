package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
)

// MaxPlaylistTracks 是网页歌单的歌曲数量上限；旧 Subsonic 整体替换契约不变。
const MaxPlaylistTracks = 2000

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
	var playlist Playlist
	var public int
	if err := tx.QueryRowContext(ctx, `SELECT p.id, p.user_id, u.name, p.name, p.comment, p.public, p.created_at, p.updated_at FROM playlists p JOIN users u ON u.id=p.user_id WHERE p.id = ?`, id).
		Scan(&playlist.ID, &playlist.UserID, &playlist.Owner, &playlist.Name, &playlist.Comment, &public, &playlist.CreatedAt, &playlist.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	playlist.Public = public == 1
	if actor == nil || (!actor.IsAdmin && actor.ID != playlist.UserID) {
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
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	playlist, err := editablePlaylistTx(ctx, tx, id, actor)
	if err != nil {
		return false, err
	}
	ids := playlist.TrackIDs
	added := !slices.Contains(ids, track.ID)
	if added && len(ids) >= MaxPlaylistTracks {
		return false, ErrPlaylistLimit
	}
	// 即便条目已经存在，也补齐早期版本可能只保存在内存里的元数据。
	if err := upsertTracksTx(ctx, tx, []Track{track}); err != nil {
		return false, err
	}
	if added {
		ids = append([]string{track.ID}, ids...)
		if err := replacePlaylistTracksTx(ctx, tx, id, ids); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return added, nil
}
