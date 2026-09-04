package backup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/secret"
)

func TestEncryptedArchiveRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	sourceDB, err := db.Open(filepath.Join(sourceDir, "lxsc.db"))
	if err != nil {
		t.Fatal(err)
	}
	sourceBox, _ := secret.Load(sourceDir, "source-key")
	enc, _ := sourceBox.Encrypt("original-password")
	user, err := sourceDB.CreateUser(ctx, "admin", enc, true, "flac")
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.CreatePlaylistFull(ctx, "pl-test", user.ID, "迁移歌单", "备注", true, nil); err != nil {
		t.Fatal(err)
	}
	davPassword, _ := sourceBox.Encrypt("dav-secret")
	if err := sourceDB.SetSetting(ctx, "backup.webdav.password", davPassword); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "config.yaml"), []byte("listen: ':9999'\nproxy: http://proxy:7890\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, "sources"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "sources", "custom.js"), []byte("// source"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(sourceDB, sourceBox, sourceDir, "test", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	archive, err := service.CreateArchive(ctx, "backup-password")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Remove()
	if archive.Manifest.FormatVersion != 1 || !archive.Manifest.Encrypted {
		t.Fatalf("清单异常: %+v", archive.Manifest)
	}

	targetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(targetDir, "config.yaml"), []byte("listen: ':8080'\ndata_dir: /target\nlog_level: warn\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	targetDB, err := db.Open(filepath.Join(targetDir, "lxsc.db"))
	if err != nil {
		t.Fatal(err)
	}
	targetBox, _ := secret.Load(targetDir, "target-key")
	targetService := New(targetDB, targetBox, targetDir, "target", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := targetService.InspectAndStage(ctx, archive.Path, "wrong", "test"); err == nil {
		t.Fatal("错误密码应失败")
	}
	if _, err := targetService.InspectAndStage(ctx, archive.Path, "backup-password", "test"); err != nil {
		t.Fatal(err)
	}
	if targetService.Status().Pending == nil {
		t.Fatal("应存在待恢复任务")
	}
	_ = targetDB.Close()
	applied, err := ApplyPending(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("应应用待恢复任务")
	}
	restoredDB, err := db.Open(filepath.Join(targetDir, "lxsc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	restored, err := restoredDB.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := targetBox.Decrypt(restored.PasswordEnc)
	if err != nil || plain != "original-password" {
		t.Fatalf("用户密码迁移失败: %q %v", plain, err)
	}
	storedDAV := restoredDB.GetSetting(ctx, "backup.webdav.password", "")
	plain, err = targetBox.Decrypt(storedDAV)
	if err != nil || plain != "dav-secret" {
		t.Fatalf("WebDAV 密码迁移失败: %q %v", plain, err)
	}
	if _, err := restoredDB.GetPlaylist(ctx, "pl-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "sources", "custom.js")); err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(filepath.Join(targetDir, "config.yaml"))
	if string(config) == "" || !contains(string(config), "listen: :8080") || !contains(string(config), "proxy: http://proxy:7890") {
		t.Fatalf("配置合并异常:\n%s", config)
	}
	if err := applied.Commit(); err != nil {
		t.Fatal(err)
	}
	if targetService.Status().Pending != nil {
		t.Fatal("恢复提交后不应保留待恢复任务")
	}
}

func TestLegacyPendingLayout(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	targetDB, err := db.Open(filepath.Join(dir, "lxsc.db"))
	if err != nil {
		t.Fatal(err)
	}
	targetBox, _ := secret.Load(dir, "target-key")
	oldEnc, _ := targetBox.Encrypt("old")
	_, _ = targetDB.CreateUser(ctx, "old-user", oldEnc, true, "320k")
	_ = targetDB.Close()

	sourceDir := t.TempDir()
	sourceDB, _ := db.Open(filepath.Join(sourceDir, "lxsc.db"))
	newEnc, _ := targetBox.Encrypt("new")
	_, _ = sourceDB.CreateUser(ctx, "new-user", newEnc, true, "320k")
	stage := filepath.Join(dir, ".restore", "staging")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := sourceDB.Snapshot(ctx, filepath.Join(stage, "lxsc.db")); err != nil {
		t.Fatal(err)
	}
	_ = sourceDB.Close()
	pending := []byte(`{"createdAt":1,"backupAt":1,"sourceVersion":"legacy","source":"旧版本"}`)
	if err := os.WriteFile(filepath.Join(dir, ".restore", "pending.json"), pending, 0o600); err != nil {
		t.Fatal(err)
	}
	applied, err := ApplyPending(dir)
	if err != nil {
		t.Fatal(err)
	}
	if applied == nil {
		t.Fatal("旧布局待恢复任务应可应用")
	}
	restored, err := db.Open(filepath.Join(dir, "lxsc.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.GetUserByName(ctx, "new-user"); err != nil {
		t.Fatalf("旧布局恢复未生效: %v", err)
	}
	_ = restored.Close()
	if err := applied.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelPending(t *testing.T) {
	dir := t.TempDir()
	database, _ := db.Open(filepath.Join(dir, "lxsc.db"))
	defer database.Close()
	box, _ := secret.Load(dir, "key")
	s := New(database, box, dir, "test", nil, nil)
	if err := os.MkdirAll(s.stagingDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.pendingPath(), []byte(`{"createdAt":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelPending(); err != nil {
		t.Fatal(err)
	}
	if s.Status().Pending != nil {
		t.Fatal("待恢复任务未取消")
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
