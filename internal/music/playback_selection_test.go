package music

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/js"
)

func TestPublishedCandidateKeepsValidationAndRejectsLateResolver(t *testing.T) {
	c := newStabilityCatalog(t)
	started, release := make(chan struct{}), make(chan struct{})
	c.urlCall = func(ctx context.Context, _ string, _ any, quality string, ids []int64) (*js.MusicURLResult, error) {
		if quality == "320k" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &js.MusicURLResult{URL: "https://media.invalid/" + quality, Quality: quality, SourceID: 1}, nil
	}
	lower, err := c.ResolvePlaybackCandidate(context.Background(), stabilityTrack(), "128k", []int64{1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var checks atomic.Int32
	check := func(context.Context) (*http.Response, error) {
		checks.Add(1)
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}
	if _, err = c.CheckPlaybackURL(context.Background(), lower, check); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.ResolvePlaybackCandidate(context.Background(), stabilityTrack(), "320k", []int64{1}, nil)
		done <- err
	}()
	<-started
	published := c.PublishPlaybackURL(stabilityTrack(), "320k", lower)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = c.CheckPlaybackURL(context.Background(), published, check); err != nil || checks.Load() != 1 {
		t.Fatal("发布不能改变媒体验证版本", checks.Load(), err)
	}
	cached, ok := c.CachedPlaybackURL(stabilityTrack(), "320k")
	if !ok || cached.Result.Quality != "128k" {
		t.Fatal("迟到取链覆盖了媒体验证赢家", cached)
	}
	c.InvalidatePlaybackURL(lower)
	if _, ok := c.CachedPlaybackURL(stabilityTrack(), "320k"); ok {
		t.Fatal("旧档位别名失败后不能保留同版本的已发布链接")
	}
	if _, err = c.CheckPlaybackURL(context.Background(), published, check); err != nil || checks.Load() != 2 {
		t.Fatal("同一解析版本的失败必须失效全部校验别名")
	}
}

func TestDowngradedURLExpirySurvivesPermanentCacheRestart(t *testing.T) {
	c := newStabilityCatalog(t)
	setURLTTL(t, c, -1)
	now := time.Now()
	c.urls.now = func() time.Time { return now }
	dir := t.TempDir()
	attachTestURLCache(t, c, dir)
	var calls atomic.Int32
	resolve := func(context.Context, string, any, string, []int64) (*js.MusicURLResult, error) {
		calls.Add(1)
		return &js.MusicURLResult{URL: "https://media.invalid/low", Quality: "128k", SourceID: 1}, nil
	}
	c.urlCall = resolve
	if _, err := c.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k"); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseURLCache(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(downgradedURLTTL - time.Millisecond)
	restarted := NewCatalog(c.DB, nil, nil, c.Settings, c.Log)
	restarted.urls.now = func() time.Time { return now }
	restarted.urlCall = resolve
	attachTestURLCache(t, restarted, dir)
	result, err := restarted.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil || !result.Cached || calls.Load() != 1 {
		t.Fatal("降级链接有效期内应复用", result, err)
	}
	now = now.Add(time.Millisecond)
	result, err = restarted.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil || result.Cached || calls.Load() != 2 {
		t.Fatal("永久缓存也必须重新竞争高档", result, err)
	}
}
