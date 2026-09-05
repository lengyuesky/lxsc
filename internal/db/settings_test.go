package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSetSettingsRollsBackPartialWrites(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := database.SetSettings(ctx, map[string]string{"a": "旧值", "b": "旧值"}); err != nil {
		t.Fatal(err)
	}
	// 按键排序确保先写入 a，再由 b 的触发器模拟中途失败。
	if _, err := database.sql.Exec(`CREATE TRIGGER fail_setting BEFORE UPDATE ON settings WHEN NEW.key = 'b' BEGIN SELECT RAISE(ABORT, '模拟写入失败'); END`); err != nil {
		t.Fatal(err)
	}
	if err := database.SetSettings(ctx, map[string]string{"a": "新值", "b": "新值"}); err == nil {
		t.Fatal("应该触发事务回滚")
	}
	if database.GetSetting(ctx, "a", "") != "旧值" || database.GetSetting(ctx, "b", "") != "旧值" {
		t.Fatal("不能部分写入设置")
	}
}
