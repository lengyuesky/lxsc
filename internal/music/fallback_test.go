package music

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"lxsc/internal/chksz"
	"lxsc/internal/js"
)

func withFallbackAPI(t *testing.T, c *Catalog, calls *atomic.Int32) {
	t.Helper()
	c.Fallback = chksz.New(&http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := `{"code":200,"data":{"url":"https://fallback.invalid/` + r.URL.Query().Get("level") + `","level":"` + r.URL.Query().Get("level") + `"}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	if _, err := c.Settings.Update(context.Background(), map[string]json.RawMessage{"fallbackAPIKey": json.RawMessage(`"test-key-123"`)}); err != nil {
		t.Fatal(err)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 兜底直链与音源直链共用媒体校验缓存，二者的解析版本不能相撞；兜底地址也不能发布进音源直链缓存。
func TestFallbackResolutionsAreIsolatedFromSourceCache(t *testing.T) {
	c := newStabilityCatalog(t)
	var calls atomic.Int32
	withFallbackAPI(t, c, &calls)
	c.urlCall = func(context.Context, string, any, string, []int64) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://media.invalid/source", Quality: "320k", SourceID: 1}, nil
	}
	source, err := c.ResolvePlaybackCandidate(context.Background(), stabilityTrack(), "320k", []int64{1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := c.ResolveFallbackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil || fallback.Result.SourceID != FallbackSourceID || fallback.Result.URL != "https://fallback.invalid/exhigh" {
		t.Fatal(fallback, err)
	}
	if source.token == fallback.token {
		t.Fatal("两个缓存签发的版本相撞")
	}
	var checks atomic.Int32
	check := func(context.Context) (*http.Response, error) {
		checks.Add(1)
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	}
	_, _ = c.CheckPlaybackAudio(context.Background(), source, true, check)
	_, _ = c.CheckPlaybackAudio(context.Background(), fallback, true, check)
	if checks.Load() != 2 {
		t.Fatal("兜底地址不能复用音源地址的校验结果")
	}

	c.PublishPlaybackURL(stabilityTrack(), "320k", fallback)
	if cached, ok := c.CachedPlaybackURL(stabilityTrack(), "320k"); ok && cached.Result.SourceID == FallbackSourceID {
		t.Fatal("兜底地址不能发布进音源直链缓存")
	}
	again, err := c.ResolveFallbackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil || !again.Cached || calls.Load() != 1 {
		t.Fatal("兜底地址应短暂缓存，避免重复消耗额度", again, calls.Load())
	}
	c.InvalidatePlaybackURL(again)
	if refreshed, err := c.ResolveFallbackURL(context.Background(), stabilityTrack(), "320k"); err != nil || refreshed.Cached || calls.Load() != 2 {
		t.Fatal("失效后应重新调用接口", refreshed, calls.Load())
	}
	setURLTTL(t, c, 0)
	_, _ = c.ResolveFallbackURL(context.Background(), stabilityTrack(), "320k")
	if again, _ := c.ResolveFallbackURL(context.Background(), stabilityTrack(), "320k"); again.Cached {
		t.Fatal("管理员关闭直链缓存时兜底地址同样不缓存")
	}
}
