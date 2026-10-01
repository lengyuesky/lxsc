package db

import (
	"context"
	"errors"
	"testing"
)

func TestAPIKeyThrottlingStillChecksRevocationAndExpiry(t *testing.T) {
	d, alice, _ := newPlaylistWriteDB(t)
	ctx := context.Background()
	if err := d.CreateAPIKey(ctx, alice.ID, "device", "测试"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetUserByAPIKey(ctx, "device"); err != nil {
		t.Fatal(err)
	}
	keys, err := d.ListAPIKeys(ctx, alice.ID)
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == 0 {
		t.Fatal(keys, err)
	}
	first := keys[0].LastUsedAt
	for range 10 {
		if _, err := d.GetUserByAPIKey(ctx, "device"); err != nil {
			t.Fatal(err)
		}
	}
	keys, _ = d.ListAPIKeys(ctx, alice.ID)
	if keys[0].LastUsedAt != first {
		t.Fatal("窗口内使用时间改变")
	}
	if _, err := d.sql.Exec(`UPDATE api_keys SET last_used_at=?`, now()-301); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetUserByAPIKey(ctx, "device"); err != nil {
		t.Fatal(err)
	}
	keys, _ = d.ListAPIKeys(ctx, alice.ID)
	if keys[0].LastUsedAt < first {
		t.Fatal("窗口过期未更新")
	}
	if _, err := d.sql.Exec(`UPDATE api_keys SET expires_at=?`, now()-1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetUserByAPIKey(ctx, "device"); !errors.Is(err, ErrNotFound) {
		t.Fatal("已过期密钥仍有效", err)
	}
	if err := d.RevokeAPIKey(ctx, alice.ID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetUserByAPIKey(ctx, "device"); !errors.Is(err, ErrNotFound) {
		t.Fatal("撤销未即时生效", err)
	}
}
