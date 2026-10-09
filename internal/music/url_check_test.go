package music

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/httpguard"
	"lxsc/internal/js"
)

func checkedURLCatalog(t *testing.T) (*Catalog, URLResolution) {
	t.Helper()
	c := newStabilityCatalog(t)
	c.urlCall = func(_ context.Context, _ string, _ any, quality string, _ []int64) (*js.MusicURLResult, error) {
		return &js.MusicURLResult{URL: "https://media.invalid/same", Quality: quality, SourceID: 1}, nil
	}
	resolution, err := c.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil {
		t.Fatal(err)
	}
	return c, resolution
}

func TestURLCheckSuccessReuseHasAbsoluteExpiryAndPrivateHeaders(t *testing.T) {
	c, resolution := checkedURLCatalog(t)
	now := time.Now()
	c.urlChecks.now = func() time.Time { return now }
	var checks atomic.Int32
	upstream := &http.Response{StatusCode: 206, Header: http.Header{
		"Content-Type": {"audio/mpeg"}, "Content-Length": {"1"}, "Content-Range": {"bytes 0-0/12345"},
		"Etag": {`"original"`}, "Set-Cookie": {"private"}, "Location": {"https://private.invalid/signed"},
	}, Request: &http.Request{Header: http.Header{"Authorization": {"private"}}}}
	check := func(context.Context) (*http.Response, error) { checks.Add(1); return upstream, nil }
	first, err := c.CheckPlaybackURL(context.Background(), resolution, check)
	if err != nil || first == nil || first.StatusCode != 206 {
		t.Fatalf("首次校验失败: %v", err)
	}
	upstream.Header.Set("ETag", `"upstream-mutated"`)
	first.Header.Set("ETag", `"caller-mutated"`)
	now = now.Add(urlCheckTTL - time.Millisecond)
	second, err := c.CheckPlaybackURL(context.Background(), resolution, check)
	if err != nil || second == nil || checks.Load() != 1 || second.Header.Get("ETag") != `"original"` || second.Header.Get("Content-Range") != "bytes 0-0/12345" {
		t.Fatalf("HEAD/GET 应复用独立的媒体头副本: checks=%d err=%v", checks.Load(), err)
	}
	if second.Request != nil || second.Body != http.NoBody || second.Header.Get("Set-Cookie") != "" || second.Header.Get("Location") != "" {
		t.Fatal("缓存不能保留请求、正文、凭据或签名地址")
	}
	now = now.Add(time.Millisecond)
	if _, err := c.CheckPlaybackURL(context.Background(), resolution, check); err != nil || checks.Load() != 2 {
		t.Fatalf("达到原始有效期必须重新校验，命中不能续期: checks=%d err=%v", checks.Load(), err)
	}
}

func TestURLCheckRefreshAndQualityNeverReuseOldValidation(t *testing.T) {
	c, old := checkedURLCatalog(t)
	var checks atomic.Int32
	check := func(context.Context) (*http.Response, error) {
		checks.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}}, nil
	}
	if _, err := c.CheckPlaybackURL(context.Background(), old, check); err != nil {
		t.Fatal(err)
	}
	fresh, err := c.RefreshPlaybackURL(context.Background(), stabilityTrack(), old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CheckPlaybackURL(context.Background(), fresh, check); err != nil || checks.Load() != 2 {
		t.Fatal("相同地址重新解析后也必须重新校验")
	}
	c.InvalidatePlaybackURL(old)
	current, err := c.ResolvePlaybackURL(context.Background(), stabilityTrack(), "320k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CheckPlaybackURL(context.Background(), current, check); err != nil || checks.Load() != 2 {
		t.Fatal("迟到的旧链接失败不能清除新版本的校验")
	}
	lower, err := c.ResolvePlaybackURL(context.Background(), stabilityTrack(), "128k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CheckPlaybackURL(context.Background(), lower, check); err != nil || checks.Load() != 3 {
		t.Fatal("不同音质不能复用媒体头")
	}
	c.InvalidateURLs()
	if c.urlChecks.stats().Entries != 0 {
		t.Fatal("音源配置失效应一并清理成功校验缓存")
	}
}

func TestURLCheckDoesNotCacheFailuresOrDisabledResults(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		status            int
		err               error
		disabled          bool
	}{
		{name: "失效", status: 403},
		{name: "上游故障", status: 503},
		{name: "错误页", status: 200, contentType: "text/html", err: httpguard.ErrNonAudioResponse},
		{name: "超时", err: context.DeadlineExceeded},
		{name: "取消", err: context.Canceled},
		{name: "关闭缓存", status: 206, contentType: "audio/mpeg", disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, resolution := checkedURLCatalog(t)
			if tc.disabled {
				setURLTTL(t, c, 0)
			}
			var checks atomic.Int32
			for range 2 {
				_, err := c.CheckPlaybackURL(context.Background(), resolution, func(context.Context) (*http.Response, error) {
					checks.Add(1)
					if tc.status == 0 {
						return nil, tc.err
					}
					return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}}}, nil
				})
				if !errors.Is(err, tc.err) {
					t.Fatalf("校验错误应保留: got=%v want=%v", err, tc.err)
				}
			}
			if checks.Load() != 2 || c.urlChecks.stats().Entries != 0 {
				t.Fatalf("此类结果不能缓存: checks=%d", checks.Load())
			}
		})
	}
}

func TestURLCheckKnownFailureDiscardsCompletedAndLateValidation(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		name := "已完成"
		if inFlight {
			name = "在途"
		}
		t.Run(name, func(t *testing.T) {
			c, resolution := checkedURLCatalog(t)
			started, release := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			var checks atomic.Int32
			go func() {
				_, err := c.CheckPlaybackURL(context.Background(), resolution, func(ctx context.Context) (*http.Response, error) {
					checks.Add(1)
					close(started)
					if inFlight {
						select {
						case <-release:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return &http.Response{StatusCode: 200}, nil
				})
				done <- err
			}()
			<-started
			if !inFlight {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			c.InvalidatePlaybackURL(resolution)
			close(release)
			if inFlight {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			response, err := c.CheckPlaybackURL(context.Background(), resolution, func(context.Context) (*http.Response, error) {
				checks.Add(1)
				return &http.Response{StatusCode: 403}, nil
			})
			if err != nil || response == nil || response.StatusCode != 403 || checks.Load() != 2 {
				t.Fatal("已知失败后，旧成功记录和迟到结果都不能跳过重新校验")
			}
		})
	}
}
