package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"lxsc/internal/config"
	"lxsc/internal/db"
	"lxsc/internal/secret"
)

func ensureAdmin(ctx context.Context, database *db.DB, box *secret.Box, cfg config.Config, log *slog.Logger) error {
	count, err := database.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("检查初始账号失败: %w", err)
	}
	if count != 0 {
		return nil
	}
	if utf8.RuneCountInString(strings.TrimSpace(cfg.AdminPassword)) < 12 || cfg.AdminPassword == cfg.AdminUser {
		return fmt.Errorf("首次启动必须通过 LXSC_ADMIN_PASSWORD 或 admin_password 设置至少 12 个字符且不同于用户名的管理员密码")
	}
	enc, err := box.Encrypt(cfg.AdminPassword)
	if err != nil {
		return fmt.Errorf("加密初始密码失败: %w", err)
	}
	if _, err := database.CreateUser(ctx, cfg.AdminUser, enc, true, "320k"); err != nil {
		return fmt.Errorf("创建管理员失败: %w", err)
	}
	log.Info("已创建管理员账号", "user", cfg.AdminUser)
	return nil
}
