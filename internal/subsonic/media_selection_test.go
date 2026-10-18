package subsonic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/diagnostics"
)

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

func TestMediaHedgeCancelsSlowHeaderAndPreservesSelectedCache(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
	addMediaFallback(t, s, 22, "b")
	var active, maximum atomic.Int32
	cancelled := make(chan struct{}, 1)
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); count > old && !maximum.CompareAndSwap(old, count); old = maximum.Load() {
		}
		if strings.HasPrefix(r.URL.Path, "/a/") {
			<-r.Context().Done()
			cancelled <- struct{}{}
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
	started := time.Now()
	r, resp, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, true)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil || r.Result.SourceID != 22 || time.Since(started) > mediaFallbackDelay+time.Second || maximum.Load() != 2 {
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
