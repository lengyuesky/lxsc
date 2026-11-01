package db

import (
	"context"
	"database/sql"
	"errors"
)

// EnsureClientID 保存可再生的协议标识，冲突时不改写另一项的映射。
func (d *DB) EnsureClientID(ctx context.Context, protocol, kind, id, guid string) error {
	gotKind, gotID, err := d.ResolveClientID(ctx, protocol, guid)
	if err == nil {
		if gotKind != kind || gotID != id {
			return errors.New("客户端标识冲突")
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `INSERT INTO client_ids(protocol,kind,internal_id,guid) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, protocol, kind, id, guid)
	if err != nil {
		return err
	}
	gotKind, gotID, err = d.ResolveClientID(ctx, protocol, guid)
	if err != nil {
		return err
	}
	if gotKind != kind || gotID != id {
		return errors.New("客户端标识冲突")
	}
	return nil
}

// ResolveClientID 只解析标识；调用方仍必须验证该资源的当前访问权限。
func (d *DB) ResolveClientID(ctx context.Context, protocol, guid string) (kind, id string, err error) {
	err = d.read.QueryRowContext(ctx, `SELECT kind,internal_id FROM client_ids WHERE protocol=? AND guid=?`, protocol, guid).Scan(&kind, &id)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}
