package settings

import (
	"context"
	"encoding/json"
	"path/filepath"
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
