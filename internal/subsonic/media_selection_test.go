package subsonic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/diagnostics"
)

// setSourceConcurrency 调整播放取链并发数；按顺序断言簿记逻辑的测试固定为 1。
func setSourceConcurrency(t *testing.T, s *Server, n int) {
	t.Helper()
	if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"sourceConcurrency": json.RawMessage(fmt.Sprint(n))}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaDowngradeAndCachedValidationForBothPolicies(t *testing.T) {
	for _, strategy := range []string{"quality", "speed"} {
		t.Run(strategy, func(t *testing.T) {
			script := `lx.on(lx.EVENT_NAMES.request,({info})=>Promise.resolve('https://media.invalid/'+info.type));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k']}}});`
			s, user, info := newMediaStabilityServer(t, script)
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"sourceSelection": json.RawMessage(fmt.Sprintf("%q", strategy))}); err != nil {
				t.Fatal(err)
			}
			var checks atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				checks.Add(1)
				status := 403
				if r.URL.Path == "/320k" {
					status = 200
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
			})}
			for round := range 2 {
				resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, true)
				if response != nil {
					response.Body.Close()
				}
				if err != nil || !resolution.Validated || resolution.Result.Quality != "320k" || checks.Load() != 3 {
					t.Fatalf("第 %d 次请求未复用降级版本: %+v checks=%d err=%v", round, resolution, checks.Load(), err)
				}
			}
			for _, state := range s.Catalog.Sources.ScheduleSnapshot(1) {
				if state.Action == "media" && state.Success != 1 {
					t.Fatal("重新发布不能重复反馈成功", state)
				}
			}
		})
	}
}

func TestFourthSourceRemainsEligible(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
	for i, label := range []string{"b", "c", "d"} {
		addMediaFallback(t, s, int64(i+2), label)
	}
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := 503
		if strings.HasPrefix(r.URL.Path, "/d/") {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	r, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, true)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil || !r.Validated || r.Result.SourceID != 4 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRejectedHighQualityTriesSameSourceLowerQuality(t *testing.T) {
	script := `lx.on(lx.EVENT_NAMES.request,({info})=>Promise.resolve('https://media.invalid/'+info.type));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k']}}});`
	s, user, info := newMediaStabilityServer(t, script)
	var paths []string
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		status := 403
		if r.URL.Path == "/320k" {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	r, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, true)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil || r.Result.Quality != "320k" || strings.Join(paths, ",") != "/flac,/flac,/320k" {
		t.Fatalf("%+v %v %v", r, err, paths)
	}
}

func TestMediaConcurrentRaceCancelsSlowHeaderAndPreservesSelectedCache(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
	addMediaFallback(t, s, 22, "b")
	var active, maximum atomic.Int32
	cancelled, aStarted := make(chan struct{}, 1), make(chan struct{})
	var aOnce sync.Once
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); count > old && !maximum.CompareAndSwap(old, count); old = maximum.Load() {
		}
		if strings.HasPrefix(r.URL.Path, "/a/") {
			aOnce.Do(func() { close(aStarted) })
			<-r.Context().Done()
			cancelled <- struct{}{}
			return nil, r.Context().Err()
		}
		// 等 A 的请求在途后再应答，确定性地验证两源同时发起。
		select {
		case <-aStarted:
		case <-time.After(time.Second):
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	started := time.Now()
	r, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, true)
	if resp != nil {
		defer resp.Body.Close()
	}
	// 两个源同时发起：B 立即胜出，不等待 A 的头部超时。
	if err != nil || r.Result.SourceID != 22 || time.Since(started) > 500*time.Millisecond || maximum.Load() != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("落败媒体请求未取消")
	}
	cached, ok := s.Catalog.CachedPlaybackURL(info, "320k")
	if !ok || cached.Result.SourceID != 22 {
		t.Fatal("未保存媒体验证的赢家")
	}
}

func TestMediaRateLimitHonorsRetryAfter(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
	var calls atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"60"}}, Body: http.NoBody}, nil
	})}
	for range 2 {
		_, _, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, true)
		if err == nil {
			t.Fatal("限流不能算可用")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("Retry-After 内重复探测", calls.Load())
	}
}

func TestConcurrentHEADGETDeduplicatesMediaHealth(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
	s.Diagnostics = &diagnostics.Events{}
	var calls atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	var workers sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		workers.Go(func() {
			resolution, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, true)
			if resp != nil {
				resp.Body.Close()
			}
			if err == nil && !resolution.Validated {
				err = fmt.Errorf("没有验证")
			}
			errors <- err
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("相同版本未合并: %d", calls.Load())
	}
	for _, state := range s.Catalog.Sources.ScheduleSnapshot(1) {
		if state.Action == "media" && (state.Samples != 1 || state.Success != 1) {
			t.Fatal("重复健康反馈", state)
		}
	}
}

// mediaQualityScript 返回按档位区分地址的音源脚本：https://media.invalid/<label>/<档位>。
func mediaQualityScript(label string, qualities ...string) string {
	list, _ := json.Marshal(qualities)
	return fmt.Sprintf(`lx.on(lx.EVENT_NAMES.request,({info})=>Promise.resolve('https://media.invalid/%s/'+info.type));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:%s}}});`, label, list)
}

func addMediaSource(t *testing.T, s *Server, id int64, script string) {
	t.Helper()
	if _, err := s.Catalog.Sources.Load(context.Background(), id, script); err != nil {
		t.Fatal(err)
	}
}

func setSourceSelection(t *testing.T, s *Server, strategy string) {
	t.Helper()
	if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"sourceSelection": json.RawMessage(fmt.Sprintf("%q", strategy))}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaQualityRacesAllSourcesPerTier(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaQualityScript("a", "flac", "320k"))
	addMediaSource(t, s, 2, mediaQualityScript("b", "flac", "320k"))
	addMediaSource(t, s, 3, mediaQualityScript("c", "flac", "320k"))
	var active, maximum, flacArrived, flacDone atomic.Int32
	var early atomic.Bool
	allFlac := make(chan struct{})
	var allFlacOnce sync.Once
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); count > old && !maximum.CompareAndSwap(old, count); old = maximum.Load() {
		}
		if strings.HasSuffix(r.URL.Path, "/flac") {
			if flacArrived.Add(1) == 3 {
				allFlacOnce.Do(func() { close(allFlac) })
			}
			// 三个源的 flac 都在途后才统一拒绝，确定性地验证同档并发。
			select {
			case <-allFlac:
			case <-time.After(time.Second):
			}
			defer flacDone.Add(1)
			return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"audio/flac"}}, Body: http.NoBody}, nil
		}
		if flacDone.Load() != 3 {
			early.Store(true)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	r, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, false)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil || !r.Validated || r.Result.Quality != "320k" {
		t.Fatalf("flac 全部失败后应降到 320k: %+v %v", r.Result, err)
	}
	if flacArrived.Load() != 3 || maximum.Load() != 3 || early.Load() {
		t.Fatalf("同档三个源必须同时在途，且全部失败后才降档: flac=%d max=%d early=%v", flacArrived.Load(), maximum.Load(), early.Load())
	}
}

func TestMediaConcurrencyLimitRefillsAfterFailure(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
	for i, label := range []string{"b", "c", "d"} {
		addMediaFallback(t, s, int64(i+2), label)
	}
	setSourceConcurrency(t, s, 2)
	var active, maximum atomic.Int32
	var aFailed, cAfterA, dRequested atomic.Bool
	bStarted, bCancelled := make(chan struct{}), make(chan struct{}, 1)
	var bOnce sync.Once
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); count > old && !maximum.CompareAndSwap(old, count); old = maximum.Load() {
		}
		switch r.URL.Path[:3] {
		case "/a/":
			// 等 B 也在途后才失败，验证上限内的两个源同时发起。
			select {
			case <-bStarted:
			case <-time.After(time.Second):
			}
			aFailed.Store(true)
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: http.NoBody}, nil
		case "/b/":
			bOnce.Do(func() { close(bStarted) })
			<-r.Context().Done()
			bCancelled <- struct{}{}
			return nil, r.Context().Err()
		case "/c/":
			cAfterA.Store(aFailed.Load())
		default:
			dRequested.Store(true)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	r, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, false)
	if resp != nil {
		resp.Body.Close()
	}
	if err != nil || r.Result.SourceID != 3 || maximum.Load() != 2 || !cAfterA.Load() || dRequested.Load() {
		t.Fatalf("并发上限为 2 时第三个源只能在有源失败后补位: %+v err=%v max=%d cAfterA=%v d=%v", r.Result, err, maximum.Load(), cAfterA.Load(), dRequested.Load())
	}
	select {
	case <-bCancelled:
	case <-time.After(time.Second):
		t.Fatal("胜出后必须取消仍在途的 B")
	}
}

func TestMediaSpeedWaitsBrieflyForHigherQuality(t *testing.T) {
	for _, tc := range []struct {
		name    string
		delay   time.Duration
		source  int64
		quality string
	}{
		{"等待窗口内的更高档胜出", 20 * time.Millisecond, 2, "flac"},
		{"超过等待窗口采用已就绪的低档", mediaQualityWait + 400*time.Millisecond, 1, "320k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaQualityScript("a", "320k"))
			addMediaSource(t, s, 2, mediaQualityScript("b", "flac", "320k"))
			setSourceSelection(t, s, "speed")
			aReady := make(chan struct{})
			var aOnce sync.Once
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/a/320k":
					defer aOnce.Do(func() { close(aReady) })
				case "/b/flac":
					// A 的低档就绪后再计时，确定性地落在等待窗口之内或之外。
					select {
					case <-aReady:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					select {
					case <-time.After(tc.delay):
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
			})}
			res, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, false)
			if resp != nil {
				resp.Body.Close()
			}
			if err != nil || res.Result.SourceID != tc.source || res.Result.Quality != tc.quality {
				t.Fatalf("speed 只为更高档额外等待 %v: %+v %v", mediaQualityWait, res.Result, err)
			}
		})
	}
}

func TestMediaValidCachedURLDoesNotRaceOtherSources(t *testing.T) {
	for _, strategy := range []string{"quality", "speed"} {
		t.Run(strategy, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			addMediaFallback(t, s, 22, "b")
			setSourceSelection(t, s, strategy)
			if _, err := s.Catalog.ResolvePlaybackURLForSources(context.Background(), info, "320k", []int64{1}, nil); err != nil {
				t.Fatal(err)
			}
			var paths []string
			var pathsMu sync.Mutex
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				pathsMu.Lock()
				paths = append(paths, r.URL.Path)
				pathsMu.Unlock()
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
			})}
			res, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, false)
			if resp != nil {
				resp.Body.Close()
			}
			if err != nil || res.Result.SourceID != 1 || !reflect.DeepEqual(paths, []string{"/a/1"}) {
				t.Fatalf("缓存有效时只校验缓存链接: %+v %v %v", res.Result, err, paths)
			}
			if calls := s.Catalog.Sources.CallStatisticsOf(22, time.Now(), time.Hour).Total.Calls; calls != 0 {
				t.Fatalf("缓存命中不能向其它源并发取链: %d", calls)
			}
		})
	}
}

func TestMediaSpeedDowngradesAfterCachedTopQualityFails(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaQualityScript("a", "flac", "320k"))
	setSourceSelection(t, s, "speed")
	if cached, err := s.Catalog.ResolvePlaybackURL(context.Background(), info, "flac"); err != nil || cached.Result.Quality != "flac" {
		t.Fatal("预热缓存失败", cached.Result, err)
	}
	var paths []string
	var pathsMu sync.Mutex
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		status := 403
		if r.URL.Path == "/a/320k" {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	res, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, false)
	if resp != nil {
		resp.Body.Close()
	}
	// 缓存轮已校验并刷新过 flac，竞速轮跳过该档，但同一音源仍须继续源内降档。
	if err != nil || !res.Validated || res.Result.Quality != "320k" || !reflect.DeepEqual(paths, []string{"/a/flac", "/a/flac", "/a/320k"}) {
		t.Fatalf("缓存的最高档失效后，唯一音源仍须源内降档: %+v %v %v", res.Result, err, paths)
	}
}
