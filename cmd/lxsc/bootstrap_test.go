package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"lxsc/internal/config"
	"lxsc/internal/db"
	"lxsc/internal/secret"
)

func TestBootstrapRequiresExplicitPasswordOnlyForNewDatabase(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	box, err := secret.Load(dir, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	for _, password := range []string{"", "admin", "short", "            "} {
		cfg.AdminPassword = password
		if ensureAdmin(ctx, database, box, cfg, log) == nil {
			t.Fatal("不应创建弱口令账户")
		}
	}
	if n, err := database.CountUsers(ctx); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	cfg.AdminPassword = "test-strong-password"
	if err := ensureAdmin(ctx, database, box, cfg, log); err != nil {
		t.Fatal(err)
	}
	cfg.AdminPassword = ""
	if err := ensureAdmin(ctx, database, box, cfg, log); err != nil {
		t.Fatal("已有账号不应强制重新初始化", err)
	}
	u, err := database.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	password, err := box.Decrypt(u.PasswordEnc)
	if err != nil || password != "test-strong-password" {
		t.Fatal("已有密码被覆盖")
	}
}
