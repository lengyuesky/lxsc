package db

import (
	"context"
	"errors"
	"time"
)

const boardSnapshotLimit = 128

// GetBoardSnapshot 读取持久化的完整榜单快照；不存在时返回 sql.ErrNoRows。
func (d *DB) GetBoardSnapshot(ctx context.Context, key string) ([]byte, int64, error) {
	var raw []byte
	var at int64
	err := d.read.QueryRowContext(ctx, `SELECT raw, updated_at FROM board_snapshots WHERE board_key=?`, key).Scan(&raw, &at)
	if err != nil {
		return nil, 0, err
	}
	return raw, at, nil
}

// PutBoardSnapshot 覆盖保存完整快照，并淘汰超出容量之外的最旧记录。
func (d *DB) PutBoardSnapshot(ctx context.Context, key string, raw []byte) error {
	if len(raw) == 0 {
		return errors.New("empty board snapshot")
	}
	if _, err := d.sql.ExecContext(ctx, `INSERT INTO board_snapshots(board_key,raw,updated_at) VALUES(?,?,?)
		ON CONFLICT(board_key) DO UPDATE SET raw=excluded.raw, updated_at=excluded.updated_at`, key, raw, time.Now().UnixMilli()); err != nil {
		return err
	}
	_, err := d.sql.ExecContext(ctx, `DELETE FROM board_snapshots WHERE board_key IN (
		SELECT board_key FROM board_snapshots ORDER BY updated_at DESC LIMIT -1 OFFSET ?)`, boardSnapshotLimit)
	return err
}

// ClearBoardSnapshots 清空全部持久化榜单快照；随元数据清理一起失效。
func (d *DB) ClearBoardSnapshots(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM board_snapshots`)
	return err
}
