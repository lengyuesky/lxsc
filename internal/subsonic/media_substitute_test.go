package subsonic

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/music"
)

// 换平台播放成功后记住替代曲目：有效期内重播直接复用，不再把所在平台的全部音源重新等一遍；
// 替代曲目不能播放时结论作废，恢复完整选源。
func TestCrossPlatformWinIsRememberedUntilSubstituteFails(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	withAlternatePlatform(t, s)
	var seen recordedPaths
	var homeWorks, substituteBroken atomic.Bool
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen.add(r.URL.Path)
		status := 500
		switch {
		case strings.HasPrefix(r.URL.Path, "/a/kg/") && !substituteBroken.Load():
			status = 200
		case strings.HasPrefix(r.URL.Path, "/a/wy/") && homeWorks.Load():
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	prepare := func() music.URLResolution {
		t.Helper()
		resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, false)
		if response != nil {
			response.Body.Close()
		}
		if err != nil || !resolution.Validated {
			t.Fatalf("应取得可播放的音频：%+v %v", resolution, err)
		}
		return resolution
	}
	if got := prepare(); got.Platform != "kg" {
		t.Fatalf("所在平台全部失败后应由 kg 胜出：%+v", got)
	}
	substitute := s.Catalog.PlaybackSubstitute(info, "320k", true)
	if substitute == nil || substitute.TrackID() != "tr-kg-alternate" {
		t.Fatalf("应记住替代曲目：%v", substitute)
	}
	if s.Catalog.PlaybackSubstitute(info, "flac", true) != nil || s.Catalog.PlaybackSubstitute(info, "320k", false) != nil {
		t.Fatal("结论只适用于已确认的音质档与验证条件")
	}

	// 清掉短时的失败结论，只留下替代曲目的记忆：即使所在平台此刻可用，也直接复用替代曲目。
	s.Catalog.InvalidateURLs()
	if s.Catalog.PlaybackSubstitute(info, "320k", true) != nil {
		t.Fatal("音源变更后旧结论应作废")
	}
	s.Catalog.RememberPlaybackSubstitute(info, "320k", true, substitute)
	homeWorks.Store(true)
	seen.paths = nil
	if got := prepare(); got.Platform != "kg" || got.TrackID != "tr-kg-alternate" {
		t.Fatalf("有效期内应直接复用替代曲目：%+v", got)
	}
	if got := seen.sorted(); !slices.Equal(got, []string{"/a/kg/320k"}) {
		t.Fatalf("复用替代曲目时不应再请求所在平台：%v", got)
	}

	// 替代曲目失效：结论作废，回到所在平台优先的完整选源。
	s.Catalog.InvalidateURLs()
	s.Catalog.RememberPlaybackSubstitute(info, "320k", true, substitute)
	substituteBroken.Store(true)
	if got := prepare(); got.Platform != "wy" {
		t.Fatalf("替代曲目失效后应恢复完整选源：%+v", got)
	}
	if s.Catalog.PlaybackSubstitute(info, "320k", true) != nil {
		t.Fatal("替代曲目失效后结论应作废")
	}
}

// 全部候选失败后的重取各档并发进行，不再按音质逐档排队。
func TestExpiredCandidatesAreRefetchedConcurrently(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	var seen recordedPaths
	var waiting atomic.Int32
	var queued atomic.Bool
	together := make(chan struct{})
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen.add(r.URL.Path)
		if count := len(seen.sorted()); count > 3 {
			// 第二轮：三档都到达后才放行；逐档排队时后面的档位永远等不到。
			if waiting.Add(1) == 3 {
				close(together)
			}
			select {
			case <-together:
			case <-time.After(2 * time.Second):
				queued.Store(true)
			}
		}
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	_, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, false)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("全部被拒绝时应报告失败")
	}
	want := []string{"/a/wy/128k", "/a/wy/128k", "/a/wy/320k", "/a/wy/320k", "/a/wy/flac", "/a/wy/flac"}
	if got := seen.sorted(); !slices.Equal(got, want) {
		t.Fatalf("被拒绝的每一档都应重取一次：%v", got)
	}
	if queued.Load() {
		t.Fatal("重取应并发进行，不应逐档排队")
	}
}
