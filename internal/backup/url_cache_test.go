package backup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/secret"
	"lxsc/internal/urlcache"
)

func cacheBackupService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "lxsc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	box, err := secret.Load(dir, "测试密钥")
	if err != nil {
		t.Fatal(err)
	}
	return New(database, box, dir, "测试", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func seedCacheFiles(t *testing.T, dir string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm", ".invalid"} {
		if err := os.WriteFile(filepath.Join(dir, urlcache.Filename)+suffix, []byte("不应备份的签名直链"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNoCacheFiles(t *testing.T, dir string) {
	t.Helper()
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(entry.Name(), urlcache.Filename) {
			t.Errorf("目录中不能保留直链缓存文件: %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArchivesExcludeURLCacheButKeepItsSetting(t *testing.T) {
	for _, password := range []string{"", "备份密码"} {
		name := "未加密"
		if password != "" {
			name = "已加密"
		}
		t.Run(name, func(t *testing.T) {
			s := cacheBackupService(t)
			seedCacheFiles(t, s.DataDir)
			ctx := context.Background()
			if err := s.DB.SetSetting(ctx, "urlCacheTTL", "-1"); err != nil {
				t.Fatal(err)
			}
			archive, err := s.CreateArchive(ctx, password)
			if err != nil {
				t.Fatal(err)
			}
			defer archive.Remove()
			for name := range archive.Manifest.Files {
				if strings.Contains(name, urlcache.Filename) {
					t.Fatalf("备份清单不能包含缓存: %s", name)
				}
			}
			payload := archive.Path
			if password != "" {
				payload = filepath.Join(t.TempDir(), "payload.tar.gz")
				if err := decryptAge(archive.Path, payload, password); err != nil {
					t.Fatal(err)
				}
			}
			extracted := t.TempDir()
			if _, err := extractAndVerify(payload, extracted); err != nil {
				t.Fatal(err)
			}
			assertNoCacheFiles(t, extracted)
			database, err := db.Open(filepath.Join(extracted, "files", "database.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			if database.GetSetting(ctx, "urlCacheTTL", "") != "-1" {
				t.Fatal("缓存档位设置仍应包含在业务备份内")
			}
		})
	}
}

func TestRestoreAndRollbackDiscardURLCache(t *testing.T) {
	source, target := cacheBackupService(t), cacheBackupService(t)
	ctx := context.Background()
	if err := source.DB.SetSetting(ctx, "serverName", "恢复后的服务"); err != nil {
		t.Fatal(err)
	}
	if err := target.DB.SetSetting(ctx, "serverName", "原服务"); err != nil {
		t.Fatal(err)
	}
	archive, err := source.CreateArchive(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Remove()
	seedCacheFiles(t, target.DataDir)
	if _, err := target.InspectAndStage(ctx, archive.Path, "", "测试"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target.DataDir, urlcache.Filename)); err != nil {
		t.Fatal("暂存恢复不能提前删除运行中缓存")
	}
	_ = target.DB.Close()
	applied, err := ApplyPending(target.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCacheFiles(t, target.DataDir)
	seedCacheFiles(t, target.DataDir)
	if err := applied.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertNoCacheFiles(t, target.DataDir)
	restored, err := db.Open(filepath.Join(target.DataDir, "lxsc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.GetSetting(ctx, "serverName", "") != "原服务" {
		t.Fatal("清理缓存不能破坏业务数据回滚")
	}
}
