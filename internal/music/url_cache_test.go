package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/urlcache"
)

func attachTestURLCache(t *testing.T, c *Catalog, dir string) {
	t.Helper()
	if err := c.EnableURLCache(dir, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseURLCache() })
}

func setURLTTL(t *testing.T, c *Catalog, ttl int) {
	t.Helper()
	if _, err := c.Settings.Update(context.Background(), map[string]json.RawMessage{"urlCacheTTL": json.RawMessage(fmt.Sprint(ttl))}); err != nil {
		t.Fatal(err)
	}
	c.RefreshTTL()
}

func TestPersistentURLCachePresetsAndAbsoluteExpiry(t *testing.T) {
	for _, ttl := range []int{0, 86400, 604800, 2592000, -1} {
		t.Run(fmt.Sprint(ttl), func(t *testing.T) {
			c := newStabilityCatalog(t)
			setURLTTL(t, c, ttl)
			dir := t.TempDir()
			now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
			c.urls.now = func() time.Time { return now }
			var calls atomic.Int32
			call := func(context.Context, string, any, string) (*js.MusicURLResult, error) {
				return &js.MusicURLResult{URL: fmt.Sprintf("https://cdn.example/%d", calls.Add(1)), Quality: "320k", Source: "测试音源"}, nil
			}
			c.urlCall = call
			attachTestURLCache(t, c, dir)
			first, err := c.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
			if err != nil || first.Cached {
				t.Fatalf("首次必须重新取链: %+v %v", first, err)
			}
			if err := c.CloseURLCache(); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			restarted := NewCatalog(c.DB, nil, nil, c.Settings, c.Log)
			restarted.urls.now = func() time.Time { return now }
			restarted.urlCall = call
			attachTestURLCache(t, restarted, dir)
			second, err := restarted.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
			if err != nil || second.Cached != (ttl != 0) || (second.Result.URL == first.Result.URL) != (ttl != 0) {
				t.Fatalf("重启后缓存行为错误: %+v %v", second, err)
			}
			if ttl > 0 {
				// 从原始解析时间计算到期，重启和读取都不能续期。
				now = now.Add(time.Duration(ttl)*time.Second - time.Hour)
			} else {
				now = now.AddDate(10, 0, 0)
			}
			third, err := restarted.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
			if err != nil || third.Cached != (ttl == -1) || (third.Result.URL == first.Result.URL) != (ttl == -1) {
				t.Fatalf("绝对过期或永久缓存错误: %+v %v", third, err)
			}
			if ttl == 0 && (calls.Load() != 3 || restarted.urls.entries.Len() != 0) {
				t.Fatal("不缓存时每次独立请求都要取链，且不能写回内存")
			}
			if _, err := c.DB.GetTrack(context.Background(), stabilityTrack().TrackID()); err == nil {
				t.Fatal("直链缓存不能隐式向主数据库写入歌曲")
			}
		})
	}
}

func TestPersistentURLCacheLRUAndDisable(t *testing.T) {
	c := newStabilityCatalog(t)
	dir := t.TempDir()
	c.urls = newRequestCache[urlKey, js.MusicURLResult](2, -time.Second)
	setURLTTL(t, c, -1)
	var calls atomic.Int32
	c.urlCall = func(context.Context, string, any, string) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: fmt.Sprintf("https://cdn.example/%d", calls.Add(1)), Quality: "320k"}, nil
	}
	attachTestURLCache(t, c, dir)
	load := func(id string) URLResolution {
		in := FromMap(map[string]any{"source": "wy", "songmid": id})
		res, err := c.ResolvePlaybackURL(context.Background(), in, "320k")
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	load("a")
	load("b")
	load("a")
	load("c")
	p := c.urls.persistent.(*urlPersistence)
	fingerprint, _ := p.fingerprint()
	records, err := p.store.Load(fingerprint, 2, time.Now())
	if err != nil || len(records) != 2 {
		t.Fatalf("数据库也必须保持容量限制: %+v %v", records, err)
	}
	for _, record := range records {
		if record.TrackID == TrackID("wy", "b") {
			t.Fatal("最久未使用的直链必须同时从数据库淘汰")
		}
	}
	setURLTTL(t, c, 0)
	load("a")
	fingerprint, _ = p.fingerprint()
	records, err = p.store.Load(fingerprint, 2, time.Now())
	if err != nil || len(records) != 0 || c.urls.entries.Len() != 0 {
		t.Fatalf("关闭缓存必须清空两层且停止写入: %+v %v", records, err)
	}
}

func TestPersistentURLCacheContextAndFaultRecovery(t *testing.T) {
	for _, change := range []string{"重命名", "脚本", "优先级", "启用状态", "代理", "重载", "持久化故障"} {
		t.Run(change, func(t *testing.T) {
			c := newStabilityCatalog(t)
			ctx := context.Background()
			source, err := c.DB.CreateSource(ctx, &db.Source{Name: "音源", Script: "原脚本", Priority: 1, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			var calls atomic.Int32
			call := func(context.Context, string, any, string) (*js.MusicURLResult, error) {
				return &js.MusicURLResult{URL: fmt.Sprintf("https://cdn.example/%d", calls.Add(1)), Quality: "320k"}, nil
			}
			c.urlCall = call
			attachTestURLCache(t, c, dir)
			old, err := c.ResolvePlaybackURL(ctx, stabilityTrack(), "320k")
			if err != nil {
				t.Fatal(err)
			}
			proxy := ""
			switch change {
			case "重命名":
				source.Name = "新名称"
			case "脚本":
				source.Script = "新脚本"
			case "优先级":
				source.Priority++
			case "启用状态":
				source.Enabled = false
			case "代理":
				proxy = "http://proxy.example:8080"
			case "重载":
				c.InvalidateURLs()
			case "持久化故障":
				p := c.urls.persistent.(*urlPersistence)
				_ = p.store.Close()
				got, err := c.ResolvePlaybackURL(ctx, stabilityTrack(), "320k")
				if err != nil || got.Result.URL != old.Result.URL || c.urls.persistent != nil {
					t.Fatal("缓存数据库故障应退化为内存缓存")
				}
				if _, err := os.Stat(filepath.Join(dir, urlcache.Filename) + ".invalid"); err != nil {
					t.Fatal("故障后必须阻止下一次启动复用旧持久记录")
				}
			}
			if err := c.DB.UpdateSource(ctx, source); err != nil {
				t.Fatal(err)
			}
			_ = c.CloseURLCache()
			restarted := NewCatalog(c.DB, nil, nil, c.Settings, c.Log)
			restarted.urlCall = call
			if err := restarted.EnableURLCache(dir, proxy); err != nil {
				t.Fatal(err)
			}
			defer restarted.CloseURLCache()
			got, err := restarted.ResolvePlaybackURL(ctx, stabilityTrack(), "320k")
			if err != nil || got.Cached != (change == "重命名") || (got.Result.URL == old.Result.URL) != (change == "重命名") {
				t.Fatalf("上下文变更或故障后缓存错误: %+v %v", got, err)
			}
		})
	}
}

func TestPersistentURLCacheLateResolutionCannotReturnToDisk(t *testing.T) {
	c := newStabilityCatalog(t)
	dir := t.TempDir()
	attachTestURLCache(t, c, dir)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c.urlCall = func(context.Context, string, any, string) (*js.MusicURLResult, error) {
		n := calls.Add(1)
		if n == 1 {
			close(started)
			<-release
		}
		return &js.MusicURLResult{URL: fmt.Sprintf("https://cdn.example/%d", n), Quality: "320k"}, nil
	}
	old := make(chan error, 1)
	go func() { _, err := c.ResolveURL(context.Background(), stabilityTrack(), "320k"); old <- err }()
	<-started
	c.InvalidateURLs()
	fresh, err := c.ResolveURL(context.Background(), stabilityTrack(), "320k")
	close(release)
	if err != nil || <-old != nil {
		t.Fatal("新旧请求均应结束")
	}
	_ = c.CloseURLCache()
	restarted := NewCatalog(c.DB, nil, nil, c.Settings, c.Log)
	attachTestURLCache(t, restarted, dir)
	got, err := restarted.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil || !got.Cached || got.Result.URL != fresh.URL {
		t.Fatalf("迟到的旧请求不能污染磁盘缓存: %+v %v", got, err)
	}
}

func TestURLChecksShareVersionAndCancelIndependently(t *testing.T) {
	c := newStabilityCatalog(t)
	c.urlCall = func(context.Context, string, any, string) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://cdn.example/same-url", Quality: "320k"}, nil
	}
	old, err := c.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int32
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	check := func(ctx context.Context) (int, error) {
		checks.Add(1)
		started <- ctx
		select {
		case <-release:
			return 403, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan error, 1)
	go func() { _, err := c.CheckPlaybackURL(ctx, old, check); cancelled <- err }()
	shared := <-started
	done := make(chan error, 19)
	for range 19 {
		go func() {
			status, err := c.CheckPlaybackURL(context.Background(), old, check)
			if err == nil && status != 403 {
				err = errors.New("共享校验结果错误")
			}
			done <- err
		}()
	}
	waitCacheWaiters(t, c.urlChecks, 20)
	cancel()
	if !errors.Is(<-cancelled, context.Canceled) || shared.Err() != nil {
		t.Fatal("单个客户端取消不能中断其他校验等待者")
	}
	fresh, err := c.RefreshPlaybackURL(context.Background(), stabilityTrack(), old)
	if err != nil {
		t.Fatal(err)
	}
	status, err := c.CheckPlaybackURL(context.Background(), fresh, func(context.Context) (int, error) {
		checks.Add(1)
		return 200, nil
	})
	if err != nil || status != 200 {
		t.Fatal("即使地址相同，新版本也不能加入旧版本校验")
	}
	close(release)
	for range 19 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if checks.Load() != 2 {
		t.Fatalf("每个版本的并发校验只执行一次: %d", checks.Load())
	}
}
