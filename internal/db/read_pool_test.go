package db

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestStatisticsReaderDoesNotBlockBusinessWrites(t *testing.T) {
	d, user, _ := listeningFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	tx, err := d.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var before string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM users WHERE id=?`, user).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateUser(ctx, user, "新名称", "", false, "320k"); err != nil {
		t.Fatalf("统计快照持有期间业务写入不应阻塞: %v", err)
	}
	if _, err := d.GetUserByID(ctx, user); err != nil {
		t.Fatalf("鉴权查询不应阻塞: %v", err)
	}
	var snapshot string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM users WHERE id=?`, user).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot != before {
		t.Fatal("同一统计事务必须保持一致快照")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := d.read.QueryRowContext(ctx, `SELECT name FROM users WHERE id=?`, user).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot != "新名称" {
		t.Fatal("下次统计必须看到最新提交")
	}
	if _, err := d.read.ExecContext(ctx, `DELETE FROM users WHERE id=?`, user); err == nil {
		t.Fatal("统计连接必须从 SQLite 层禁止写入")
	}
}
