package subsonic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
	"lxsc/internal/music"
	"lxsc/internal/testaudio"
)

func TestRealAudioCheckRejectsFastFakeAndSelectsOtherSource(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint(proxy), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			s.Catalog.AudioCheck = nil
			addMediaFallback(t, s, 22, "b")
			setSourceConcurrency(t, s, 1)
			var reads atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := []byte("fLaC encrypted-or-invalid-audio")
				if strings.HasPrefix(req.URL.Path, "/b/") {
					body = testaudio.WAV()
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/x-ogg"}}, Body: &countedMediaBody{Reader: bytes.NewReader(body), reads: &reads, closed: &atomic.Int32{}}, Request: req}, nil
			})}
			r, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", proxy, false)
			if err != nil || !r.Validated || r.Result.SourceID != 22 || reads.Load() == 0 {
				t.Fatalf("假音频抢赢或未读取音频：%+v %v", r, err)
			}
			defer response.Body.Close()
			if proxy {
				body, err := io.ReadAll(response.Body)
				if err != nil || !bytes.Equal(body, testaudio.WAV()) {
					t.Fatal("预检吞掉了代理正文前缀", err)
				}
			}
			if response.Header.Get("Content-Type") != "audio/wav" {
				t.Fatal("未按真实解码内容纠正媒体类型")
			}
			for _, item := range s.Catalog.Sources.ScheduleSnapshot(1) {
				if item.Action == "media" && item.Success != 0 {
					t.Fatal("假音频获得了成功评分")
				}
			}
		})
	}
}

func TestBadLosslessAudioStillTriesSameScriptMP3(t *testing.T) {
	for _, strategy := range []string{"quality", "speed"} {
		t.Run(strategy, func(t *testing.T) {
			script := `lx.on(lx.EVENT_NAMES.request,({info})=>Promise.resolve('https://media.invalid/'+info.type));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k']}}});`
			s, user, info := newMediaStabilityServer(t, script)
			s.Catalog.AudioCheck = nil
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"sourceSelection": json.RawMessage(fmt.Sprintf("%q", strategy))}); err != nil {
				t.Fatal(err)
			}
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := []byte("fLaC invalid content")
				if req.URL.Path == "/320k" {
					body = testaudio.MP3
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/x-ogg"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
			})}
			resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, "&quality=flac"), info, "flac", false, true)
			if response != nil {
				defer response.Body.Close()
			}
			if err != nil || !resolution.Validated || resolution.Result.Quality != "320k" || response.Header.Get("Content-Type") != "audio/mpeg" {
				t.Fatalf("无损坏内容阻止了同源MP3降档：%+v %v", resolution, err)
			}
		})
	}
}

func TestHeaderDiagnosticDoesNotReadAudioOrSeedVerifiedPlayback(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
	s.Catalog.AudioCheck = nil
	var reads, closed atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: &countedMediaBody{Reader: strings.NewReader("not audio"), reads: &reads, closed: &closed}, Request: req}, nil
	})}
	result, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: "stream", Method: "HEAD", Format: "json", Params: map[string]string{"id": info.TrackID(), "quality": "320k"}})
	if err != nil || result.Status != 200 || result.Validation != "headers_only" || reads.Load() != 0 {
		t.Fatal("诊断越过取头边界，或把取头当作音频验证", result, err)
	}
	for _, state := range s.Catalog.Sources.ScheduleSnapshot(1) {
		if state.Action == "media" && state.Success > 0 {
			t.Fatal("取头探测增加了媒体成功评分")
		}
	}
	resolution, response, err := s.prepareMedia(context.Background(), mediaStabilityRequest(user, info, ""), info, "320k", false, true)
	if response != nil {
		response.Body.Close()
	}
	if err == nil || resolution.Validated || reads.Load() == 0 {
		t.Fatal("正常播放复用了只取头的假验证", resolution, err)
	}
}

func TestPrivateAudioProbeVerifiesContentWithoutPlayingOrPersisting(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			s.Catalog.AudioCheck = nil
			var reads, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := []byte("fLaC invalid content")
				if valid {
					body = testaudio.FLAC
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/x-ogg"}}, Body: &countedMediaBody{Reader: bytes.NewReader(body), reads: &reads, closed: &closed}, Request: req}, nil
			})}
			result, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: "stream", Method: "HEAD", Format: "json", Params: map[string]string{"id": info.TrackID()}, VerifyAudio: true})
			if err != nil || reads.Load() == 0 || result.Body != nil {
				t.Fatal("音频探测没有读取片段或转发了正文", result, err)
			}
			if valid && (result.Validation != "validated" || result.Status != 200 || result.Headers["Content-Type"] != "audio/flac") {
				t.Fatal("真实音频没有通过实际解码验证", result)
			}
			if !valid && (result.Validation != "failed" || result.ProtocolStatus != "failed" || result.Error != "invalid_audio") {
				t.Fatal("假音频被当成验证成功", result)
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("主动音频探测记录了播放歌曲")
			}
		})
	}
}

func TestDecodedQQHTTPURLRemainsDirectWithoutAudioForwarding(t *testing.T) {
	script := `lx.on(lx.EVENT_NAMES.request,()=>Promise.resolve('http://cdn.example/audio.flac'));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{tx:{type:'music',actions:['musicUrl'],qualitys:['flac']}}});`
	s, user, _ := newMediaStabilityServer(t, script)
	s.Catalog.AudioCheck = nil
	info := music.FromMap(map[string]any{"source": "tx", "songmid": "synthetic", "name": "合成QQ音频", "types": []any{map[string]any{"type": "flac"}}})
	s.Catalog.Cache([]*music.Info{info})
	var reads, closed atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Range") != "bytes=0-262143" {
			t.Error("直链验证没有使用有界音频片段")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/x-ogg"}}, Body: &countedMediaBody{Reader: bytes.NewReader(testaudio.FLAC), reads: &reads, closed: &closed}, Request: req}, nil
	})}
	request := httptest.NewRequest(http.MethodGet, "/rest/stream.view?f=json&id="+info.TrackID()+"&quality=flac", nil)
	request = request.WithContext(withUser(request.Context(), user))
	response := httptest.NewRecorder()
	s.stream(response, request)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "http://cdn.example/audio.flac" || bytes.Contains(response.Body.Bytes(), testaudio.FLAC) {
		t.Fatal("QQ或HTTP直链被改为本站音频交付", response.Code, response.Header())
	}
	if reads.Load() == 0 || closed.Load() != 1 {
		t.Fatal("直链没有实际验证内容或未关闭预检响应")
	}
}

func TestDecodedProxySeekValidatesBeginningAndPreservesRequestedRange(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	s.Catalog.AudioCheck = nil
	wav := testaudio.WAV()
	var startChecks, rangeChecks atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, contentRange := wav, fmt.Sprintf("bytes 0-%d/%d", len(wav)-1, len(wav))
		switch req.Header.Get("Range") {
		case "bytes=0-262143":
			startChecks.Add(1)
		case "bytes=10-19":
			rangeChecks.Add(1)
			if req.Header.Get("If-Range") != `"版本"` {
				t.Error("拖动请求丢失了原版本条件")
			}
			body, contentRange = wav[10:20], fmt.Sprintf("bytes 10-19/%d", len(wav))
		default:
			t.Errorf("意外的范围请求：%s", req.Header.Get("Range"))
		}
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Type": {"audio/x-ogg"}, "Content-Range": {contentRange}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	for range 2 {
		request := mediaStabilityRequest(user, info, "&proxy=1")
		resolution, response, err := s.prepareMedia(context.Background(), request, info, "320k", true, false)
		if err != nil || !resolution.Validated {
			t.Fatal("有真实可解码起始片段的拖动请求被拒绝", err)
		}
		recorder := httptest.NewRecorder()
		if !s.proxyStream(recorder, request, info, resolution, response) || !bytes.Equal(recorder.Body.Bytes(), wav[10:20]) || recorder.Header().Get("Content-Type") != "audio/wav" || recorder.Header().Get("Content-Range") != fmt.Sprintf("bytes 10-19/%d", len(wav)) {
			t.Fatal("拖动响应的正文或媒体头被验证过程破坏")
		}
	}
	if startChecks.Load() != 1 || rangeChecks.Load() != 2 {
		t.Fatal("同版本拖动没有复用真实解码验证，或正式响应被错误合并")
	}
}

func TestPartialProxyWithoutContentRangeStillValidatesBeginning(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	s.Catalog.AudioCheck = nil
	wav := testaudio.WAV()
	var startChecks atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := wav[10:20]
		if req.Header.Get("Range") == "bytes=0-262143" {
			startChecks.Add(1)
			body = wav
		}
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Type": {"audio/x-ogg"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	request := mediaStabilityRequest(user, info, "&proxy=1")
	resolution, response, err := s.prepareMedia(context.Background(), request, info, "320k", true, false)
	if err != nil || !resolution.Validated || startChecks.Load() != 1 {
		t.Fatal("缺少范围头时没有独立验证原文件起始片段", err)
	}
	recorder := httptest.NewRecorder()
	if !s.proxyStream(recorder, request, info, resolution, response) || !bytes.Equal(recorder.Body.Bytes(), wav[10:20]) || recorder.Header().Get("Content-Range") != "" {
		t.Fatal("缺失的范围信息被编造，或请求的中间片段被破坏")
	}
}

func TestProxyRangeErrorDoesNotBecomeValidatedAudio(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	s.Catalog.AudioCheck = nil
	s.Diagnostics = &diagnostics.Events{}
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 416, Header: http.Header{"Content-Range": {"bytes */16044"}}, Body: http.NoBody, Request: req}, nil
	})}
	recorder := httptest.NewRecorder()
	s.stream(recorder, mediaStabilityRequest(user, info, "&proxy=1"))
	if recorder.Code != 416 {
		t.Fatal("范围错误没有保持原始响应", recorder.Code)
	}
	found := false
	for _, event := range s.Diagnostics.List() {
		if event.Stage == "proxy_response" {
			found = true
			if event.Validation != "failed" {
				t.Fatal("范围错误没有解码却被记录为音频验证成功", event.Validation)
			}
		}
	}
	if !found {
		t.Fatal("缺少代理响应诊断")
	}
	for _, state := range s.Catalog.Sources.ScheduleSnapshot(1) {
		if state.Action == "media" && state.Success > 0 {
			t.Fatal("范围错误获得了媒体成功评分")
		}
	}
}

type failingAudioWriter struct{ *httptest.ResponseRecorder }

func (w failingAudioWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDecodedProxyDistinguishesUpstreamFailureFromClientWriteFailure(t *testing.T) {
	for _, upstreamFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(upstreamFailure), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			s.Catalog.AudioCheck = nil
			// 起始片段真实可解码，只有验证上限之后的上游正文发生错误。
			prefix := append(testaudio.WAV(), make([]byte, httpguard.AudioSampleLimit-len(testaudio.WAV()))...)
			var requests atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests.Add(1)
				body := io.Reader(bytes.NewReader(prefix))
				if upstreamFailure {
					body = io.MultiReader(body, brokenMediaReader{})
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/wav"}}, Body: io.NopCloser(body), Request: req}, nil
			})}
			request := mediaStabilityRequest(user, info, "&proxy=1")
			resolution, response, err := s.prepareMedia(context.Background(), request, info, "320k", true, false)
			if err != nil || !resolution.Validated {
				t.Fatal("合成音频起始片段没有通过解码", err)
			}
			writer := http.ResponseWriter(httptest.NewRecorder())
			if !upstreamFailure {
				writer = failingAudioWriter{httptest.NewRecorder()}
			}
			if s.proxyStream(writer, request, info, resolution, response) || requests.Load() != 1 {
				t.Fatal("正文交付失败被当成成功，或交付开始后重新取链")
			}
			_, cached := s.Catalog.CachedPlaybackURL(info, "320k")
			if cached == upstreamFailure {
				t.Fatal("上游读取失败没有撤销缓存，或客户端写出失败误罚了音源")
			}
			var failures int
			for _, state := range s.Catalog.Sources.ScheduleSnapshot(1) {
				if state.Action == "media" {
					failures += state.Failed
				}
			}
			if (failures > 0) != upstreamFailure {
				t.Fatal("媒体失败评分没有区分上游读取与客户端写出")
			}
		})
	}
}
