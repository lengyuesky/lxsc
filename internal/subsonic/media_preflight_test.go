package subsonic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/diagnostics"
)

func TestMediaHEADRoutesRespectModeWithoutPlayback(t *testing.T) {
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		for _, endpoint := range []string{"stream", "stream.view", "download", "download.view"} {
			t.Run(mode+"/"+endpoint, func(t *testing.T) {
				s, user, info := newMediaStabilityServer(t, recoveryScript)
				s.Diagnostics = &diagnostics.Events{}
				if err := s.DB.CreateAPIKey(context.Background(), user.ID, "synthetic-preflight-key", "test"); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(mode)
				if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
					t.Fatal(err)
				}
				var calls, reads, closed atomic.Int32
				s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					wantMethod := http.MethodGet
					if mode == "proxy" {
						wantMethod = http.MethodHead
					} else if req.Header.Get("Range") != "bytes=0-0" {
						t.Error("重定向预检只能请求最小 Range")
					}
					if req.Method != wantMethod || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
						t.Errorf("上游预检方法或凭据隔离错误: %s", req.Method)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"audio/mpeg"}, "Content-Length": {"123456"}, "Accept-Ranges": {"bytes"}}, Body: &countedMediaBody{Reader: strings.NewReader("不得读取"), reads: &reads, closed: &closed}, Request: req}, nil
				})}
				params := url.Values{"apiKey": {"synthetic-preflight-key"}, "id": {info.TrackID()}, "c": {"Amcfy Music"}, "f": {"json"}}
				if mode == "force_redirect" {
					params.Set("proxy", "1")
				}
				router := chi.NewRouter()
				router.Mount("/rest", s.Routes())
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/rest/"+endpoint+"?"+params.Encode(), nil))
				wantStatus := http.StatusFound
				if mode == "proxy" {
					wantStatus = http.StatusOK
				}
				if rec.Code != wantStatus {
					t.Fatalf("媒体 HEAD 预检状态错误: got=%d want=%d", rec.Code, wantStatus)
				}
				if rec.Body.Len() != 0 || reads.Load() != 0 || calls.Load() != 1 || closed.Load() != 1 {
					t.Fatalf("HEAD 不得读取或转发正文，必须关闭上游: body=%d reads=%d calls=%d closed=%d", rec.Body.Len(), reads.Load(), calls.Load(), closed.Load())
				}
				if mode == "proxy" {
					if rec.Header().Get("Content-Type") != "audio/mpeg" || rec.Header().Get("Content-Length") != "123456" || rec.Header().Get("Accept-Ranges") != "bytes" || rec.Header().Get("Location") != "" {
						t.Fatal("代理 HEAD 必须返回实际媒体头")
					}
				} else if rec.Header().Get("Location") != "https://cdn.example/1" {
					t.Fatal("重定向 HEAD 必须保持原播放方式")
				}
				if rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("预检响应不能缓存")
				}
				if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
					t.Fatal("HEAD 预检不能持久化播放歌曲")
				}
				events := s.Diagnostics.List()
				last := events[len(events)-1]
				encoded, _ := json.Marshal(last)
				if last.Stage != "client_response" || last.Endpoint != strings.TrimSuffix(endpoint, ".view") || last.Client != "amcfy" || last.Result != "ok" || last.Status != wantStatus || !strings.Contains(string(encoded), `"method":"HEAD"`) {
					t.Fatalf("必须单独记录客户端 HEAD 预检: %+v", last)
				}
			})
		}
	}
}

func TestMediaHEADStillRequiresAuthenticationAndRejectsMutations(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	s.Diagnostics = &diagnostics.Events{}
	if err := s.DB.CreateAPIKey(context.Background(), user.ID, "synthetic-preflight-key", "test"); err != nil {
		t.Fatal(err)
	}
	s.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("认证失败或非媒体 HEAD 不得请求音源")
		return nil, nil
	})}
	router := chi.NewRouter()
	router.Mount("/rest", s.Routes())
	for _, key := range []string{"", "wrong-key"} {
		params := url.Values{"apiKey": {key}, "id": {info.TrackID()}, "c": {"Amcfy"}, "f": {"json"}}
		req := httptest.NewRequest(http.MethodHead, "/rest/stream.view?"+params.Encode(), nil)
		req.Header.Set("Authorization", "Bearer lxdbg_synthetic_not_a_player_key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code == http.StatusMethodNotAllowed || rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
			t.Fatal("媒体 HEAD 必须进入原有 Subsonic 认证")
		}
		events := s.Diagnostics.List()
		last := events[len(events)-1]
		if last.Result != "failed" || last.ProtocolCode == nil {
			t.Fatal("HEAD 认证失败必须保留协议诊断")
		}
	}
	for _, endpoint := range []string{"star", "scrobble", "deletePlaylist", "savePlayQueue", "getSong"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/rest/"+endpoint+"?apiKey=synthetic-preflight-key&id="+info.TrackID(), nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("不应为非媒体接口增加 HEAD 行为: %s %d", endpoint, rec.Code)
		}
	}
}

func TestMediaRejectsNonAudioSuccessResponses(t *testing.T) {
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		for _, allRejected := range []bool{false, true} {
			name := mode + "/切换可用音源"
			if allRejected {
				name = mode + "/所有地址返回错误页"
			}
			t.Run(name, func(t *testing.T) {
				s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
				addMediaFallback(t, s, 22, 2, "b")
				s.Diagnostics = &diagnostics.Events{}
				raw, _ := json.Marshal(mode)
				if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
					t.Fatal(err)
				}
				// 缓存命中也不能将 HTTP 200 的错误页继续交给播放器。
				if _, err := s.Catalog.ResolvePlaybackURL(context.Background(), info, "320k"); err != nil {
					t.Fatal(err)
				}
				var paths []string
				var badReads, badClosed atomic.Int32
				s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					paths = append(paths, req.URL.Path)
					if strings.HasPrefix(req.URL.Path, "/a/") || allRejected {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html; private=NEVER_EXPORT"}}, Body: &countedMediaBody{Reader: strings.NewReader("PRIVATE_ERROR_PAGE"), reads: &badReads, closed: &badClosed}, Request: req}, nil
					}
					return &http.Response{StatusCode: 206, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("合成音频")), Request: req}, nil
				})}
				rec := httptest.NewRecorder()
				s.stream(rec, mediaStabilityRequest(user, info, ""))
				if !reflect.DeepEqual(paths, []string{"/a/1", "/b/1"}) || badReads.Load() != 0 {
					t.Fatalf("错误页必须关闭并切源，不能读取正文或判为网络不确定: %v reads=%d", paths, badReads.Load())
				}
				if allRejected {
					if badClosed.Load() != 2 || rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
						t.Fatal("所有响应都是错误页时必须明确失败")
					}
					if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
						t.Fatal("错误页不能记录为播放成功")
					}
				} else if mode == "proxy" {
					if rec.Code != 206 || rec.Body.String() != "合成音频" {
						t.Fatal("必须转发备选音频")
					}
				} else if rec.Code != 302 || rec.Header().Get("Location") != "https://media.invalid/b/1" {
					t.Fatal("必须重定向至备选音频")
				}
				found := false
				for _, event := range s.Diagnostics.List() {
					if event.Status == 200 && event.Error == "non_audio" {
						found = true
					}
				}
				events, _ := json.Marshal(s.Diagnostics.List())
				if !found || strings.Contains(string(events)+rec.Body.String(), "NEVER_EXPORT") || strings.Contains(rec.Body.String(), "PRIVATE_ERROR_PAGE") {
					t.Fatal("诊断必须区分非音频响应且不能泄露上游正文或 MIME 参数")
				}
			})
		}
	}
}
