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

// 所在平台整体持续失败时，换平台不再等所在平台全部失败：两路同时选源，先可播放的一路胜出。
func TestFailingHomePlatformRacesCrossPlatform(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	withAlternatePlatform(t, s)
	var homeCancelled atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/a/wy/") {
			// 所在平台的音频地址迟迟不响应，只能等到被取消。
			<-r.Context().Done()
			homeCancelled.Add(1)
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	for range 3 {
		s.Catalog.RecordHomePlatform("wy", true, false)
	}
	started := time.Now()
	resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, false)
	if response != nil {
		response.Body.Close()
	}
	if err != nil || !resolution.Validated || resolution.Platform != "kg" || resolution.TrackID != "tr-kg-alternate" {
		t.Fatalf("应由其它平台胜出：%+v %v", resolution, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("不应等所在平台的候选耗尽：%v", elapsed)
	}
	if substitute := s.Catalog.PlaybackSubstitute(info, "320k", true); substitute == nil || substitute.TrackID() != "tr-kg-alternate" {
		t.Fatalf("平台持续失败时换平台胜出也应记住替代曲目：%v", substitute)
	}
	deadline := time.Now().Add(2 * time.Second)
	for homeCancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if homeCancelled.Load() == 0 {
		t.Fatal("落败的所在平台选源应被取消")
	}
	if !s.Catalog.HomePlatformFailing("wy", true) {
		t.Fatal("被取消的所在平台选源没有结论，不应改变平台状态")
	}
}

// 并行时所在平台先可播放就用所在平台，并清除平台持续失败的结论。
func TestFailingHomePlatformRecoversWhenHomeWins(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	withAlternatePlatform(t, s)
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/a/kg/") {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	for range 3 {
		s.Catalog.RecordHomePlatform("wy", true, false)
	}
	resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, false)
	if response != nil {
		response.Body.Close()
	}
	if err != nil || resolution.Platform != "wy" {
		t.Fatalf("所在平台可播放时应由所在平台胜出：%+v %v", resolution, err)
	}
	if s.Catalog.HomePlatformFailing("wy", true) || s.Catalog.PlaybackSubstitute(info, "320k", true) != nil {
		t.Fatal("所在平台恢复后应清除持续失败结论，且不记替代曲目")
	}
}

// 所在平台按歌曲确认全部失败才累计；连续达到阈值后下一首直接并行换平台。
func TestHomePlatformFailureStreakComesFromConfirmedSelections(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	prepare := func(track *music.Info, quality string) {
		t.Helper()
		_, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, track, "&quality="+quality), track, quality, false, false)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("全部候选失败应报告失败")
		}
	}
	prepare(info, "320k")
	// 刚确认过的曲目被直接跳过，重试不重复计数。
	prepare(info, "320k")
	prepare(info, "320k")
	if s.Catalog.HomePlatformFailing("wy", true) {
		t.Fatal("同一首歌的重试不应累计平台失败")
	}
	for _, id := range []string{"second", "third"} {
		track := music.FromMap(map[string]any{"source": "wy", "songmid": id, "name": "临时歌曲" + id, "types": []any{map[string]any{"type": "320k"}}})
		s.Catalog.Cache([]*music.Info{track})
		prepare(track, "320k")
	}
	if !s.Catalog.HomePlatformFailing("wy", true) || s.Catalog.HomePlatformFailing("wy", false) {
		t.Fatal("连续三首确认失败后应判定直连条件下的平台整体失败")
	}
}

// 直接跳过的已确认失败曲目不能重新记一遍：否则降档重试会压低结论的音质档，
// 客户端持续重试时结论也会无限续期。
func TestSkippedExhaustedTrackIsNotRemarked(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	var seen recordedPaths
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen.add(r.URL.Path)
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	prepare := func(quality string) {
		t.Helper()
		_, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality="+quality), info, quality, false, false)
		if response != nil {
			response.Body.Close()
		}
		if err == nil {
			t.Fatal("全部候选失败应报告失败")
		}
	}
	prepare("flac")
	confirmed := len(seen.sorted())
	prepare("128k")
	prepare("flac")
	if got := seen.sorted(); len(got) != confirmed {
		t.Fatalf("已确认 flac 失败后，降档重试不应让 flac 重新整轮选源：%v", got)
	}
}
