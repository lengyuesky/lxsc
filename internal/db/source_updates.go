package db

import (
	"context"
	"database/sql"
	"errors"
)

var ErrSourceConflict = errors.New("音源设置已变化，请重新读取后重试")

func sourceChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrSourceConflict
	}
	return nil
}

// SaveSourceConfig 原子保存管理员编辑，并阻止旧更新请求覆盖新的配置。
func (d *DB) SaveSourceConfig(ctx context.Context, previous, edited *Source) error {
	checked, result, problem, slot, attempts, retry := previous.LastCheckedAt, previous.UpdateResult, previous.UpdateError, previous.UpdateSlot, previous.UpdateAttempts, previous.RetryAt
	if edited.URL != previous.URL {
		checked, result, problem, slot, attempts, retry = 0, "", "", "", 0, 0
	}
	updated := previous.ScriptUpdatedAt
	if edited.Script != previous.Script {
		updated = now()
	}
	return sourceChanged(d.sql.ExecContext(ctx, `UPDATE sources SET name=?, description=?, version=?, author=?, homepage=?, script=?, enabled=?, updated_at=?,
		url=?, auto_update=?, last_checked_at=?, script_updated_at=?, update_result=?, update_error=?, update_slot=?, update_attempts=?, retry_at=?, update_revision=update_revision+1
		WHERE id=? AND update_revision=?`, edited.Name, edited.Description, edited.Version, edited.Author, edited.Homepage, edited.Script, boolInt(edited.Enabled), now(),
		edited.URL, boolInt(edited.AutoUpdate), checked, updated, result, problem, slot, attempts, retry, previous.ID, previous.UpdateRevision))
}

// TouchSourceRevision 将显式重载记为新操作，使在途自动更新失效。
func (d *DB) TouchSourceRevision(ctx context.Context, source *Source) error {
	return sourceChanged(d.sql.ExecContext(ctx, `UPDATE sources SET update_revision=update_revision+1 WHERE id=? AND update_revision=?`, source.ID, source.UpdateRevision))
}

type SourceCheck struct {
	Slot      string
	CheckedAt int64
	Result    string
	Error     string
	Attempts  int
	RetryAt   int64
	Script    *Source
}

// CompleteSourceCheck 在同一条写入中保存结果与经过验证的新脚本。
// 名称、启用状态和来源配置始终由管理员编辑维护。
func (d *DB) CompleteSourceCheck(ctx context.Context, previous *Source, check SourceCheck) error {
	fields := `last_checked_at=?, update_result=?, update_error=?, update_slot=?, update_attempts=?, retry_at=?, update_revision=update_revision+1`
	args := []any{check.CheckedAt, check.Result, check.Error, check.Slot, check.Attempts, check.RetryAt}
	if script := check.Script; script != nil {
		fields += `, script=?, description=?, version=?, author=?, homepage=?, script_updated_at=?, updated_at=?`
		args = append(args, script.Script, script.Description, script.Version, script.Author, script.Homepage, check.CheckedAt, check.CheckedAt)
	}
	args = append(args, previous.ID, previous.UpdateRevision)
	return sourceChanged(d.sql.ExecContext(ctx, `UPDATE sources SET `+fields+` WHERE id=? AND update_revision=?`, args...))
}
