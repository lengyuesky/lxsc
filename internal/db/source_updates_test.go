package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestSourceUpdateMigrationPreservesLegacyScripts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source, err := d.CreateSource(ctx, &Source{Name: "原有音源", Script: "原有脚本", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE legacy_sources(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',version TEXT NOT NULL DEFAULT '',author TEXT NOT NULL DEFAULT '',homepage TEXT NOT NULL DEFAULT '',script TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 1,priority INTEGER NOT NULL DEFAULT 100,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL)`,
		`INSERT INTO legacy_sources SELECT id,name,description,version,author,homepage,script,enabled,priority,created_at,updated_at FROM sources`,
		`DROP TABLE sources`, `ALTER TABLE legacy_sources RENAME TO sources`, `DELETE FROM schema_migrations WHERE version>=7`,
	} {
		if _, err := d.sql.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.GetSource(ctx, source.ID)
	if err != nil || got.Name != source.Name || got.Script != source.Script || !got.Enabled || got.URL != "" || got.AutoUpdate || got.LastCheckedAt != 0 || got.ScriptUpdatedAt != 0 {
		t.Fatalf("旧音源迁移改变了已有数据或虚构更新历史: %+v %v", got, err)
	}
}

func TestSourceUpdateRevisionAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source, err := d.CreateSource(ctx, &Source{Name: "管理员名称", Script: "旧脚本", Enabled: true, URL: "https://source.invalid/old.js", AutoUpdate: true, LastCheckedAt: 123, UpdateResult: "imported", UpdateSlot: "2026-11-11"})
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := d.ListSourceSummaries(ctx)
	if err != nil || len(summaries) != 1 || summaries[0].Script != "" || summaries[0].URL != source.URL {
		t.Fatal("调度摘要读取了脚本或丢失来源配置")
	}
	edited := *source
	edited.URL, edited.AutoUpdate = "https://source.invalid/new.js", false
	if err := d.SaveSourceConfig(ctx, source, &edited); err != nil {
		t.Fatal(err)
	}
	stale := SourceCheck{Slot: "2026-11-11", CheckedAt: 200, Result: "updated", Attempts: 1, Script: &Source{Script: "过期请求的新脚本"}}
	if err := d.CompleteSourceCheck(ctx, source, stale); !errors.Is(err, ErrSourceConflict) {
		t.Fatal("旧请求覆盖了新配置", err)
	}
	current, err := d.GetSource(ctx, source.ID)
	if err != nil || current.Script != "旧脚本" || current.Name != source.Name || !current.Enabled || current.AutoUpdate || current.LastCheckedAt != 0 || current.UpdateSlot != "" {
		t.Fatalf("保存来源修改了音源状态或保留了旧来源的检查历史: %+v %v", current, err)
	}
	if err := d.CompleteSourceCheck(ctx, current, SourceCheck{Slot: "2026-11-12", CheckedAt: 250, Result: "failed", Error: "下载失败", Attempts: 2, RetryAt: 2050}); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	current, err = d.GetSource(ctx, source.ID)
	if err != nil || current.UpdateSlot != "2026-11-12" || current.UpdateAttempts != 2 || current.RetryAt != 2050 || current.UpdateError != "下载失败" || current.URL != edited.URL || current.Script != "旧脚本" {
		t.Fatalf("重启丢失了重试状态或改变旧脚本: %+v %v", current, err)
	}
	if _, err := d.sql.Exec(`CREATE TRIGGER reject_source_script BEFORE UPDATE OF script ON sources BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	stale.Script.Script = "保存失败的新脚本"
	if err := d.CompleteSourceCheck(ctx, current, stale); err == nil {
		t.Fatal("保存失败未向调用方报告")
	}
	unchanged, err := d.GetSource(ctx, source.ID)
	if err != nil || unchanged.Script != current.Script || unchanged.LastCheckedAt != current.LastCheckedAt || unchanged.UpdateResult != current.UpdateResult {
		t.Fatal("保存失败后脚本或检查状态没有保持原样")
	}
}
