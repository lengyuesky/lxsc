package subsonic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"lxsc/internal/chksz"
	"lxsc/internal/db"
	"lxsc/internal/music"
)

// chkszFixture 模拟 ChKSz API：按接口返回 https://fallback.invalid/<平台>/<ID>/<音质参数> 形式的地址。
type chkszFixture struct {
	calls  atomic.Int32
	status atomic.Int32 // 非 0 时所有调用返回该状态
	paths  recordedPaths
}

func (f *chkszFixture) install(t *testing.T, s *Server, key string) {
	t.Helper()
	s.Catalog.Fallback = chksz.New(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.calls.Add(1)
		q := r.URL.Query()
		if q.Get("apikey") != "test-key" {
			t.Errorf("兜底请求未携带密钥：%s", r.URL.Path)
		}
		reply := func(status int, body string) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		if status := f.status.Load(); status != 0 {
			return reply(int(status), `{"code":`+strconv.Itoa(int(status))+`}`)
		}
		var address string
		switch r.URL.Path {
		case "/api/163_music":
			address = "https://fallback.invalid/wy/" + q.Get("id") + "/" + q.Get("level")
			f.paths.add(address)
			return reply(200, `{"code":200,"data":{"url":"`+address+`","level":"`+q.Get("level")+`"}}`)
		case "/api/qq_music":
			address = "https://fallback.invalid/tx/" + q.Get("mid") + "/" + q.Get("size")
		case "/api/kugou_music":
			address = "https://fallback.invalid/kg/" + q.Get("id") + "/" + q.Get("size")
		default:
			t.Errorf("未知兜底接口：%s", r.URL.Path)
		}
		f.paths.add(address)
		return reply(200, `{"code":200,"url":"`+address+`","bitrate":"`+q.Get("size")+`"}`)
	})})
	if key != "" {
		if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"fallbackAPIKey": json.RawMessage(`"` + key + `"`)}); err != nil {
			t.Fatal(err)
		}
	}
}

// mediaStatus 让音源给出的地址全部失败，兜底地址按 fallbackOK 决定能否播放。
func mediaStatus(seen *recordedPaths, fallbackOK *atomic.Bool) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen.add(r.URL.Host + r.URL.Path)
		status := 500
		if r.URL.Host == "fallback.invalid" && fallbackOK.Load() {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
	})}
}

func prepareOnce(s *Server, user *db.User, info *music.Info, quality string) (music.URLResolution, error) {
	resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality="+quality), info, quality, false, false)
	if response != nil {
		response.Body.Close()
	}
	return resolution, err
}

// 全部音源和平台都确认失败后才调用兜底；兜底地址同样要通过音频验证，
// 不进入音源直链缓存，重播时直接复用缓存的兜底地址，不再把失败的音源重新等一遍。
func TestChKSzRescuesPlaybackAfterEverySourceFails(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	var api chkszFixture
	api.install(t, s, "test-key")
	var seen recordedPaths
	var fallbackOK atomic.Bool
	fallbackOK.Store(true)
	s.HTTP = mediaStatus(&seen, &fallbackOK)

	got, err := prepareOnce(s, user, info, "320k")
	if err != nil || !got.Validated || got.Result.SourceID != music.FallbackSourceID || got.Result.Source != chksz.SourceName || got.Platform != "wy" {
		t.Fatalf("应由兜底接口取得可播放的地址：%+v %v", got, err)
	}
	if got.Result.URL != "https://fallback.invalid/wy/temporary/exhigh" || got.Result.Quality != "320k" || api.calls.Load() != 1 {
		t.Fatalf("兜底应按所在平台与请求音质调用一次：%+v calls=%d", got.Result, api.calls.Load())
	}
	sourceTried := false
	for _, path := range seen.sorted() {
		sourceTried = sourceTried || strings.HasPrefix(path, "media.invalid/a/wy/")
	}
	if !sourceTried {
		t.Fatal("兜底之前必须先试过音源")
	}
	if _, ok := s.Catalog.CachedPlaybackURL(info, "320k"); ok {
		t.Fatal("兜底地址不能进入音源直链缓存")
	}
	if remembered := s.Catalog.PlaybackFallback(info, "320k", true); remembered == nil || remembered.TrackID() != info.TrackID() {
		t.Fatal("音源全部确认失败后兜底成功应记住结论")
	}

	seen.paths = nil
	again, err := prepareOnce(s, user, info, "320k")
	if err != nil || again.Result.SourceID != music.FallbackSourceID || !again.Cached || api.calls.Load() != 1 {
		t.Fatalf("重播应复用缓存的兜底地址，不再消耗额度：%+v calls=%d %v", again, api.calls.Load(), err)
	}
	for _, path := range seen.sorted() {
		if !strings.HasPrefix(path, "fallback.invalid/") {
			t.Fatalf("结论有效期内重播不应再请求失败的音源：%v", seen.sorted())
		}
	}
}

// 兜底地址失效时向接口重新取一次；仍不能播放则作废结论并报告失败。
// 代理模式每次都实际打开音频，不复用短时的校验结果。
func TestChKSzFallbackFailureForgetsConclusion(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	var api chkszFixture
	api.install(t, s, "test-key")
	var seen recordedPaths
	var fallbackOK atomic.Bool
	fallbackOK.Store(true)
	s.HTTP = mediaStatus(&seen, &fallbackOK)
	prepare := func() (music.URLResolution, error) {
		resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", true, false)
		if response != nil {
			response.Body.Close()
		}
		return resolution, err
	}
	if got, err := prepare(); err != nil || got.Result.SourceID != music.FallbackSourceID || s.Catalog.PlaybackFallback(info, "320k", false) == nil {
		t.Fatalf("代理模式同样应由兜底播放并记住结论：%+v %v", got, err)
	}
	fallbackOK.Store(false)
	if _, err := prepare(); err == nil {
		t.Fatal("兜底地址也不能播放时应报告失败")
	}
	if s.Catalog.PlaybackFallback(info, "320k", false) != nil {
		t.Fatal("兜底失效后结论应作废")
	}
	if calls := api.calls.Load(); calls != 2 {
		t.Fatalf("缓存的兜底地址失效后应重新取一次：calls=%d", calls)
	}
}

// 音源可以播放时绝不调用兜底接口；未配置密钥时不启用兜底。
func TestChKSzIsOnlyUsedAsLastResort(t *testing.T) {
	t.Run("音源可用", func(t *testing.T) {
		s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
		var api chkszFixture
		api.install(t, s, "test-key")
		s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: http.NoBody}, nil
		})}
		got, err := prepareOnce(s, user, info, "320k")
		if err != nil || got.Result.SourceID != 1 || api.calls.Load() != 0 {
			t.Fatalf("音源可用时不应调用兜底：%+v calls=%d %v", got, api.calls.Load(), err)
		}
	})
	t.Run("未配置密钥", func(t *testing.T) {
		s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
		var api chkszFixture
		api.install(t, s, "")
		var seen recordedPaths
		var fallbackOK atomic.Bool
		fallbackOK.Store(true)
		s.HTTP = mediaStatus(&seen, &fallbackOK)
		if _, err := prepareOnce(s, user, info, "320k"); err == nil || api.calls.Load() != 0 {
			t.Fatalf("未配置密钥时不应兜底：calls=%d %v", api.calls.Load(), err)
		}
	})
}

// 兜底接口不支持酷我、咪咕：改用其它平台上的同一首歌。
func TestChKSzUsesSupportedPlatformForUnsupportedTrack(t *testing.T) {
	s, user, _ := newMediaStabilityServer(t, exhaustiveScript("a"))
	withAlternatePlatform(t, s)
	info := music.FromMap(map[string]any{"source": "kw", "songmid": "kwsong", "name": "临时歌曲", "singer": "测试歌手"})
	s.Catalog.Cache([]*music.Info{info})
	var api chkszFixture
	api.install(t, s, "test-key")
	var seen recordedPaths
	var fallbackOK atomic.Bool
	fallbackOK.Store(true)
	s.HTTP = mediaStatus(&seen, &fallbackOK)
	got, err := prepareOnce(s, user, info, "320k")
	if err != nil || got.Result.SourceID != music.FallbackSourceID || got.Platform != "kg" || got.TrackID != "tr-kg-alternate" {
		t.Fatalf("应改用 kg 上的同一首歌兜底：%+v %v", got, err)
	}
	if paths := api.paths.sorted(); len(paths) != 1 || paths[0] != "https://fallback.invalid/kg/alternate/320k" {
		t.Fatalf("兜底调用不符：%v", paths)
	}
}

// 密钥失效后暂停兜底，后续播放不再调用接口。
func TestChKSzAuthFailurePausesFallback(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, exhaustiveScript("a"))
	var api chkszFixture
	api.install(t, s, "test-key")
	api.status.Store(401)
	var seen recordedPaths
	var fallbackOK atomic.Bool
	s.HTTP = mediaStatus(&seen, &fallbackOK)
	if _, err := prepareOnce(s, user, info, "320k"); err == nil {
		t.Fatal("密钥失效时应报告失败")
	}
	if api.calls.Load() != 1 || s.Catalog.FallbackEnabled() {
		t.Fatalf("密钥失效后应暂停兜底：calls=%d", api.calls.Load())
	}
	s.Catalog.InvalidateURLs()
	if _, err := prepareOnce(s, user, info, "320k"); err == nil || api.calls.Load() != 1 {
		t.Fatalf("暂停期间不应再调用接口：calls=%d", api.calls.Load())
	}
}
