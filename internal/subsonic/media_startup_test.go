package subsonic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMediaHEADAndPlaybackReuseOneSuccessfulProbe(t *testing.T) {
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			raw, _ := json.Marshal(mode)
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
				t.Fatal(err)
			}
			var probes, transfers, reads, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				headers := http.Header{"Content-Type": {"audio/mpeg"}, "ETag": {`"media"`}}
				body := "0123456789"
				if req.Header.Get("Range") == "bytes=0-0" {
					probes.Add(1)
					headers.Set("Content-Length", "1")
					headers.Set("Content-Range", "bytes 0-0/100")
					body = body[:1]
				} else {
					transfers.Add(1)
					if req.Header.Get("Range") != "bytes=10-19" || req.Header.Get("If-Range") != `"版本"` {
						t.Error("代理正式播放必须保留客户端范围和条件头")
					}
					headers.Set("Content-Length", "10")
					headers.Set("Content-Range", "bytes 10-19/100")
				}
				return &http.Response{StatusCode: 206, Header: headers, Body: &countedMediaBody{Reader: strings.NewReader(body), reads: &reads, closed: &closed}, Request: req}, nil
			})}
			for round := range 2 {
				head := mediaStabilityRequest(user, info, "")
				head.Method = http.MethodHead
				rec := httptest.NewRecorder()
				s.stream(rec, head)
				if mode == "force_redirect" {
					if rec.Code != 302 || rec.Header().Get("Location") == "" {
						t.Fatal("强制重定向模式的 HEAD 契约不能改变")
					}
				} else if rec.Code != 200 || rec.Header().Get("Content-Length") != "100" || rec.Header().Get("Accept-Ranges") != "bytes" {
					t.Fatalf("预检应返回完整大小及已证明支持的范围能力: %d %v", rec.Code, rec.Header())
				}
				if rec.Body.Len() != 0 || rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("HEAD 不得返回正文或允许客户端长期缓存")
				}
				if round == 0 {
					if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
						t.Fatal("预检不能记录播放")
					}
				}
				rec = httptest.NewRecorder()
				s.stream(rec, mediaStabilityRequest(user, info, ""))
				if mode == "proxy" {
					if rec.Code != 206 || rec.Body.String() != "0123456789" || rec.Header().Get("Content-Range") != "bytes 10-19/100" {
						t.Fatal("代理 GET 必须请求并转发实际音频，不能复用一字节预检作为正文")
					}
				} else if rec.Code != 302 || rec.Header().Get("Location") == "" {
					t.Fatal("正式播放应继续交付直链")
				}
			}
			if probes.Load() != 1 {
				t.Fatalf("连续 HEAD/GET 只应发起一次上游预检: %d", probes.Load())
			}
			wantTransfers := int32(0)
			if mode == "proxy" {
				wantTransfers = 2
			} else if reads.Load() != 0 {
				t.Fatal("重定向模式不得读取音频正文")
			}
			if transfers.Load() != wantTransfers || closed.Load() != probes.Load()+transfers.Load() {
				t.Fatal("正式音频传输不能合并，所有上游响应都必须关闭")
			}
		})
	}
}

func TestMediaProxyFailureAfterHEADInvalidatesCheckedVersion(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	var probes, transfers atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := 206
		if req.Header.Get("Range") == "bytes=0-0" {
			probes.Add(1)
		} else {
			transfers.Add(1)
			if req.URL.Path == "/1" {
				status = 403
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("audio")), Request: req}, nil
	})}
	head := mediaStabilityRequest(user, info, "")
	head.Method = http.MethodHead
	s.stream(httptest.NewRecorder(), head)
	play := httptest.NewRecorder()
	s.stream(play, mediaStabilityRequest(user, info, "&proxy=1"))
	if play.Code != 206 || play.Body.String() != "audio" || transfers.Load() != 2 {
		t.Fatal("实际媒体返回 403 时必须重新取链，不能相信先前预检")
	}
	s.stream(httptest.NewRecorder(), head)
	if probes.Load() != 2 {
		t.Fatal("刷新后的直链必须重新获取媒体头")
	}
}

func TestMediaDeferredRefreshStillRecoversWhenBackupFails(t *testing.T) {
	for _, mode := range []string{"redirect", "proxy"} {
		for _, failure := range []string{"媒体失败", "解析失败"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
				backup := mediaFailoverScript("b")
				if failure == "解析失败" {
					backup = strings.Replace(backup, "Promise.resolve", "Promise.reject", 1)
				}
				if _, err := s.Catalog.Sources.Load(context.Background(), 22, 2, backup); err != nil {
					t.Fatal(err)
				}
				var paths []string
				s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					paths = append(paths, req.URL.Path)
					status := 503
					if req.URL.Path == "/a/1" {
						status = 403
					} else if req.URL.Path == "/a/2" {
						status = 200
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/mpeg"}}, Body: io.NopCloser(strings.NewReader("audio")), Request: req}, nil
				})}
				extra := ""
				if mode == "proxy" {
					extra = "&proxy=1"
				}
				rec := httptest.NewRecorder()
				s.stream(rec, mediaStabilityRequest(user, info, extra))
				want := []string{"/a/1", "/b/1", "/a/2"}
				if failure == "解析失败" {
					want = []string{"/a/1", "/a/2"}
				}
				if !reflect.DeepEqual(paths, want) {
					t.Fatalf("备用不可用时仍须补做原源刷新: got=%v want=%v", paths, want)
				}
				if mode == "proxy" {
					if rec.Code != 200 || rec.Body.String() != "audio" {
						t.Fatal("刷新后应正常转发音频")
					}
				} else if rec.Code != 302 || rec.Header().Get("Location") != "https://media.invalid/a/2" {
					t.Fatal("刷新后应交付已验证的原源新链接")
				}
			})
		}
	}
}
