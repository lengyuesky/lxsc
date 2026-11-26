package settings

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"lxsc/internal/db"
)

func TestCustomAPIDefaultsPersistenceAndDisable(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := New(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().CustomLyricsURL != "https://api.lrc.cx/lyrics" || s.Get().CustomCoverURL != "https://api.lrc.cx/cover" {
		t.Fatal("默认应使用用户指定的公共 LrcAPI")
	}
	for _, value := range []string{"https://custom.example/api?name={title}&artist={artist}", ""} {
		raw, _ := json.Marshal(value)
		if _, err := s.Update(ctx, map[string]json.RawMessage{"customLyricsURL": raw, "customCoverURL": raw}); err != nil {
			t.Fatal(err)
		}
		reloaded, err := New(ctx, d)
		if err != nil || reloaded.Get().CustomLyricsURL != value || reloaded.Get().CustomCoverURL != value {
			t.Fatal("自定义地址或关闭状态未持久保存", err)
		}
	}
	if _, err := s.Update(ctx, map[string]json.RawMessage{"serverName": json.RawMessage(`"不应写入"`), "customCoverURL": json.RawMessage(`"http://127.0.0.1/private"`)}); err == nil || s.Get().ServerName != "lxsc" {
		t.Fatal("无效接口不能导致其他设置部分保存")
	}
}

// ChKSz 密钥只写不读：JSON 只公开是否已配置，空字符串关闭兜底。
func TestFallbackAPIKeyIsWriteOnly(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := New(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if s.Get().FallbackAPIKey != "" || s.Get().FallbackAPIKeySet {
		t.Fatal("默认不应启用兜底接口")
	}
	updated, err := s.Update(ctx, map[string]json.RawMessage{"fallbackAPIKey": json.RawMessage(`"  sk-abc123  "`)})
	if err != nil || updated.FallbackAPIKey != "sk-abc123" || !updated.FallbackAPIKeySet {
		t.Fatal("密钥应去除首尾空白后保存", updated.FallbackAPIKey, err)
	}
	encoded, _ := json.Marshal(updated)
	if strings.Contains(string(encoded), "sk-abc123") || !strings.Contains(string(encoded), `"fallbackAPIKeySet":true`) {
		t.Fatalf("设置 JSON 不能包含密钥明文：%s", encoded)
	}
	reloaded, err := New(ctx, d)
	if err != nil || reloaded.Get().FallbackAPIKey != "sk-abc123" {
		t.Fatal("密钥未持久保存", err)
	}
	for _, bad := range []string{`"含 空格"`, `"中文"`, `123`, `"` + strings.Repeat("k", 257) + `"`} {
		if _, err := s.Update(ctx, map[string]json.RawMessage{"fallbackAPIKey": json.RawMessage(bad)}); err == nil {
			t.Fatal("应拒绝无效密钥", bad)
		}
	}
	if _, err := s.Update(ctx, map[string]json.RawMessage{"fallbackAPIKeySet": json.RawMessage(`false`)}); err == nil {
		t.Fatal("是否已配置是只读字段")
	}
	cleared, err := s.Update(ctx, map[string]json.RawMessage{"fallbackAPIKey": json.RawMessage(`""`)})
	if err != nil || cleared.FallbackAPIKey != "" || cleared.FallbackAPIKeySet {
		t.Fatal("空字符串应关闭兜底", err)
	}
}
