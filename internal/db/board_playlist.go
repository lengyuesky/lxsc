package db

import (
	"context"
	"database/sql"
	"errors"
)

// BoardPlaylistContent 只接受完整榜单的摘要，Hash 包含歌曲顺序与元数据。
type BoardPlaylistContent struct {
	Count    int
	Duration int
	Hash     string
}

// BoardPlaylistMetadata 与快照分开保存；快照过期、淘汰或重启不应丢失已知摘要。
type BoardPlaylistMetadata struct {
	Name        string
	Comment     string
	Count       int
	Duration    int
	ContentHash string
	UpdatedAt   int64
}

const boardPlaylistMetadataQuery = `SELECT name,comment,song_count,duration,content_hash,updated_at FROM board_playlist_metadata WHERE board_id=?`

func scanBoardPlaylistMetadata(row *sql.Row) (BoardPlaylistMetadata, error) {
	meta := BoardPlaylistMetadata{Count: 1}
	err := row.Scan(&meta.Name, &meta.Comment, &meta.Count, &meta.Duration, &meta.ContentHash, &meta.UpdatedAt)
	return meta, err
}

func mergeBoardPlaylistMetadata(meta BoardPlaylistMetadata, name, comment string, content *BoardPlaylistContent) BoardPlaylistMetadata {
	// 直接打开详情时名称缓存可能尚未恢复，不能用榜单 ID 覆盖上次已知名称。
	if name != "" {
		meta.Name = name
	}
	meta.Comment = comment
	if content != nil {
		meta.Count, meta.Duration, meta.ContentHash = content.Count, content.Duration, content.Hash
	}
	return meta
}

// SyncBoardPlaylistMetadata 仅在名称、简介或完整内容变化时推进版本时间。
// 同内容的高频读取走只读连接；需要写入时在事务内复查，避免并发请求反复推进版本。
func (d *DB) SyncBoardPlaylistMetadata(ctx context.Context, id, name, comment string, content *BoardPlaylistContent) (BoardPlaylistMetadata, error) {
	if content != nil && (content.Count <= 0 || content.Duration < 0 || content.Hash == "") {
		return BoardPlaylistMetadata{}, errors.New("无效的完整榜单摘要")
	}
	meta, err := scanBoardPlaylistMetadata(d.read.QueryRowContext(ctx, boardPlaylistMetadataQuery, id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BoardPlaylistMetadata{}, err
	}
	if err == nil && mergeBoardPlaylistMetadata(meta, name, comment, content) == meta {
		return meta, nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return BoardPlaylistMetadata{}, err
	}
	defer tx.Rollback()
	meta, err = scanBoardPlaylistMetadata(tx.QueryRowContext(ctx, boardPlaylistMetadataQuery, id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BoardPlaylistMetadata{}, err
	}
	next := mergeBoardPlaylistMetadata(meta, name, comment, content)
	if err == nil && next == meta {
		return meta, tx.Commit()
	}
	// Subsonic 的时间按秒输出，同一秒内两次真实变化也必须可区分。
	next.UpdatedAt = max(now(), meta.UpdatedAt+1)
	_, err = tx.ExecContext(ctx, `INSERT INTO board_playlist_metadata(board_id,name,comment,song_count,duration,content_hash,updated_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(board_id) DO UPDATE SET name=excluded.name,comment=excluded.comment,song_count=excluded.song_count,duration=excluded.duration,content_hash=excluded.content_hash,updated_at=excluded.updated_at`,
		id, next.Name, next.Comment, next.Count, next.Duration, next.ContentHash, next.UpdatedAt)
	if err != nil {
		return BoardPlaylistMetadata{}, err
	}
	if err := tx.Commit(); err != nil {
		return BoardPlaylistMetadata{}, err
	}
	return next, nil
}
