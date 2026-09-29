package music

import (
	"context"
	"testing"
	"time"

	"lxsc/internal/js"
)

func TestPersistentCacheTouchIsThrottledAcrossRestart(t *testing.T) {
	c := newStabilityCatalog(t)
	dir := t.TempDir()
	at := time.Now()
	c.urls.now = func() time.Time { return at }
	c.urlCall = func(context.Context, string, any, string, []int64) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://example.invalid/audio", Quality: "320k", SourceID: 1}, nil
	}
	attachTestURLCache(t, c, dir)
	load := func(c *Catalog) {
		t.Helper()
		if _, err := c.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k"); err != nil {
			t.Fatal(err)
		}
	}
	lastUsed := func(c *Catalog) int64 {
		t.Helper()
		p := c.urls.persistent.(*urlPersistence)
		fingerprint, err := p.fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		records, err := p.store.Load(fingerprint, 2000, at)
		if err != nil || len(records) != 1 {
			t.Fatalf("读取持久缓存失败: %v", err)
		}
		return records[0].LastUsedAt
	}
	load(c)
	initial := lastUsed(c)
	at = at.Add(59 * time.Second)
	for range 100 {
		load(c)
	}
	if lastUsed(c) != initial {
		t.Fatal("一分钟内命中不能反复写入使用时间")
	}
	at = at.Add(time.Second)
	load(c)
	updated := lastUsed(c)
	if updated != at.UnixNano() {
		t.Fatal("窗口结束后应持久化最近使用时间")
	}
	if err := c.CloseURLCache(); err != nil {
		t.Fatal(err)
	}
	restarted := NewCatalog(c.DB, nil, nil, c.Settings, c.Log)
	restarted.urls.now = func() time.Time { return at }
	attachTestURLCache(t, restarted, dir)
	at = at.Add(time.Second)
	load(restarted)
	if lastUsed(restarted) != updated {
		t.Fatal("重启恢复使用时间后仍应节流")
	}
}
