package subsonic

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

func TestProtocolProbeReadsBothFormatsAndRejectsWrites(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	for _, method := range []string{"GET", "POST"} {
		for _, format := range []string{"json", "xml"} {
			result, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: "getSong", Method: method, Format: format, Params: map[string]string{"id": info.TrackID()}})
			if err != nil || result.Status != 200 || result.ProtocolStatus != "ok" || result.Format != format || result.Body == nil {
				t.Fatalf("协议探测失败: %+v %v", result, err)
			}
		}
	}
	for _, endpoint := range []string{"scrobble", "star", "setRating", "deletePlaylist", "savePlayQueue", "../api/admin/users"} {
		if _, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: endpoint, Method: "POST", Format: "json"}); err == nil {
			t.Fatal("探测允许了写入", endpoint)
		}
	}
	for _, credential := range []string{"u", "p", "t", "s", "apiKey", "Authorization", "Cookie", "callback", "f"} {
		if _, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: "getSong", Method: "GET", Format: "json", Params: map[string]string{credential: "PRIVATE"}}); err == nil {
			t.Fatal("不能接受真实凭据或回调", credential)
		}
	}
	for _, format := range []string{"json", "xml"} {
		result, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: "getPlaylist", Method: "GET", Format: format, Params: map[string]string{"id": "pl-missing"}})
		if err != nil || result.Status != 200 || result.ProtocolStatus != "failed" || result.ProtocolCode == nil {
			t.Fatal("HTTP200不能掩盖协议失败", result, err)
		}
	}
}

func TestPlaybackProbeNeverTransfersOrPersistsAudio(t *testing.T) {
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			t.Run(mode+"/"+method, func(t *testing.T) {
				s, user, info := newMediaStabilityServer(t, recoveryScript)
				s.Diagnostics = &diagnostics.Events{}
				raw, _ := json.Marshal(mode)
				if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
					t.Fatal(err)
				}
				var reads, closed atomic.Int32
				s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 206, Header: http.Header{"Content-Type": {"audio/mpeg"}, "Content-Length": {"1"}, "Content-Range": {"bytes 0-0/100"}}, Body: &countedMediaBody{Reader: strings.NewReader("不得读取音频"), reads: &reads, closed: &closed}, Request: req}, nil
				})}
				before, _ := s.DB.Statistics(context.Background())
				result, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: "stream", Method: method, Format: "json", Params: map[string]string{"id": info.TrackID()}})
				if err != nil || result.SourceID != 1 || result.Quality != "320k" || result.Mode != mode {
					t.Fatal(result, err)
				}
				want := 302
				if method == "HEAD" && mode != "force_redirect" {
					want = 200
				} else if mode == "proxy" {
					want = 206
				}
				if result.Status != want || reads.Load() != 0 || closed.Load() != 1 {
					t.Fatal("播放探测只能返回响应头", result.Status, reads.Load(), closed.Load())
				}
				if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
					t.Fatal("探测不能记录播放歌曲")
				}
				after, _ := s.DB.Statistics(context.Background())
				if !reflect.DeepEqual(before, after) || len(s.Diagnostics.List()) != 0 {
					t.Fatal("探测不能写播放统计或冒充真实客户端事件")
				}
			})
		}
	}
}

func TestProtocolPreviewBound(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	info.Raw["name"] = strings.Repeat("大", 100000)
	s.Catalog.Cache([]*music.Info{info})
	result, err := s.ProbeProtocol(context.Background(), user, diagnostics.ProtocolRequest{Endpoint: "getSong", Method: "GET", Format: "json", Params: map[string]string{"id": info.TrackID()}})
	if err != nil || !result.Truncated || result.ProtocolStatus != "ok" {
		t.Fatal(result.Truncated, err)
	}
	preview, ok := result.Body.(string)
	if !ok || len(preview) != probePreviewLimit {
		t.Fatal("协议预览必须有界", len(preview))
	}
}
