package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStreamModeProxyOverrideMatrix(t *testing.T) {
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		for _, proxy := range []string{"", "0", "1"} {
			for _, endpoint := range []string{"stream", "download"} {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					t.Run(mode+"/proxy="+proxy+"/"+endpoint+"/"+method, func(t *testing.T) {
						s, user, info := newMediaStabilityServer(t, recoveryScript)
						raw, err := json.Marshal(mode)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
							t.Fatal(err)
						}
						var calls atomic.Int32
						s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
							calls.Add(1)
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("音频")), Request: req}, nil
						})}
						params := url.Values{"id": {info.TrackID()}, "f": {"json"}}
						if proxy != "" {
							params.Set("proxy", proxy)
						}
						path := "/rest/" + endpoint + ".view"
						var req *http.Request
						if method == http.MethodPost {
							req = httptest.NewRequest(method, path, strings.NewReader(params.Encode()))
							req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
						} else {
							req = httptest.NewRequest(method, path+"?"+params.Encode(), nil)
						}
						req.Header.Set("Range", "bytes=10-19")
						req.Header.Set("If-Range", `"版本"`)
						req = req.WithContext(withUser(req.Context(), user))
						rec := httptest.NewRecorder()
						if endpoint == "stream" {
							s.stream(rec, req)
						} else {
							s.download(rec, req)
						}
						wantProxy := mode == "proxy" || (mode == "redirect" && proxy == "1")
						if wantProxy {
							if calls.Load() != 1 || rec.Code != http.StatusOK || rec.Body.String() != "音频" {
								t.Fatalf("应保留原有代理行为: calls=%d status=%d body=%s", calls.Load(), rec.Code, rec.Body.String())
							}
						} else if calls.Load() != 1 || rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://cdn.example/1" {
							t.Fatalf("首次取链应轻量校验后返回302，不转发音频: calls=%d status=%d headers=%v", calls.Load(), rec.Code, rec.Header())
						}
						if rec.Header().Get("Cache-Control") != "no-store" {
							t.Fatal("媒体响应不应被缓存")
						}
						_, storedErr := s.DB.GetTrack(context.Background(), info.TrackID())
						if (storedErr == nil) != (endpoint == "stream") {
							t.Fatalf("仅成功播放应持久化歌曲，下载不应持久化: %v", storedErr)
						}
					})
				}
			}
		}
	}
}

func TestForceRedirectResolutionFailureNeverProxies(t *testing.T) {
	const script = `
lx.on(lx.EVENT_NAMES.request, () => Promise.reject(new Error('测试取链失败')))
lx.send(lx.EVENT_NAMES.inited, {status:true, sources:{wy:{name:'测试',type:'music',actions:['musicUrl'],qualitys:['320k']}}})`
	for _, endpoint := range []string{"stream", "download"} {
		t.Run(endpoint, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, script)
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"force_redirect"`)}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, errors.New("不应请求音频上游")
			})}
			rec := httptest.NewRecorder()
			req := mediaStabilityRequest(user, info, "&proxy=1")
			if endpoint == "stream" {
				s.stream(rec, req)
			} else {
				s.download(rec, req)
			}
			if calls.Load() != 0 || rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
				t.Fatalf("取链失败应返回协议错误，不得回退代理: calls=%d headers=%v body=%s", calls.Load(), rec.Header(), rec.Body.String())
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("取链失败不应持久化歌曲")
			}
		})
	}
}
