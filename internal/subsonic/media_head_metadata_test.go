package subsonic

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/diagnostics"
)

func TestMediaHEADWorksWhenPlayableUpstreamRejectsHEAD(t *testing.T) {
	for _, mode := range []string{"redirect", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			audio := strings.Repeat("0123456789", 10)
			var upstreamHEADs atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					upstreamHEADs.Add(1)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Accept-Ranges", "bytes")
				if value := r.Header.Get("Range"); value != "" {
					if value != "bytes=0-0" || r.Header.Get("If-Range") != "" || r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("预检只能请求最小 Range，不能继承客户端的范围和条件头")
					}
					w.Header().Set("Content-Length", "1")
					w.Header().Set("Content-Range", "bytes 0-0/100")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, audio[:1])
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
				_, _ = io.WriteString(w, audio)
			}))
			defer upstream.Close()
			script := strings.ReplaceAll(recoveryScript, "https://cdn.example", upstream.URL)
			s, user, info := newMediaStabilityServer(t, script)
			s.HTTP = upstream.Client()
			if err := s.DB.CreateAPIKey(context.Background(), user.ID, "synthetic-head-metadata-key", "test"); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(mode)
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
				t.Fatal(err)
			}
			router := chi.NewRouter()
			router.Mount("/rest", s.Routes())
			server := httptest.NewServer(router)
			defer server.Close()
			params := url.Values{"apiKey": {"synthetic-head-metadata-key"}, "id": {info.TrackID()}, "c": {"Amcfy Music"}, "f": {"json"}}
			address := server.URL + "/rest/stream.view?" + params.Encode()
			// 真实客户端跟随 302，先证明 GET 已能完整播放。
			play, err := server.Client().Get(address)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(play.Body)
			_ = play.Body.Close()
			if err != nil || play.StatusCode != 200 || string(body) != audio {
				t.Fatalf("GET 应已能播放: status=%d err=%v", play.StatusCode, err)
			}
			req, _ := http.NewRequest(http.MethodHead, address, nil)
			req.Header.Set("Range", "bytes=35-44")
			req.Header.Set("If-Range", `"旧版本"`)
			head, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(head.Body)
			_ = head.Body.Close()
			if err != nil || head.StatusCode != http.StatusOK || head.Header.Get("Content-Type") != "audio/mpeg" || head.ContentLength != int64(len(audio)) {
				t.Fatalf("可播放音频的 HEAD 应返回媒体元数据: status=%d type=%q length=%d err=%v", head.StatusCode, head.Header.Get("Content-Type"), head.ContentLength, err)
			}
			if len(body) != 0 || head.Header.Get("Location") != "" || head.Header.Get("Content-Range") != "" || upstreamHEADs.Load() != 0 {
				t.Fatal("预检不能把客户端继续跳转到不支持 HEAD 的音源，也不能把一字节探测当作完整资源")
			}
		})
	}
}

func TestMediaHEADReportsFullLengthWithoutReading(t *testing.T) {
	for _, tc := range []struct {
		name, contentRange, contentLength, wantLength string
		status                                        int
	}{
		{"完整长度", "bytes 0-0/123456", "1", "123456", 206},
		{"忽略Range的完整响应", "", "123456", "123456", 200},
		{"未知完整长度", "bytes 0-0/*", "1", "", 206},
		{"分块响应无长度", "", "", "", 200},
		{"片段长度不能当作总长", "invalid", "1", "", 206},
		{"范围越界", "bytes 0-10/10", "11", "", 206},
		{"有符号长度非法", "bytes 0-0/+100", "1", "", 206},
		{"长度溢出", "bytes 0-0/9223372036854775808", "1", "", 206},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			var reads, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				headers := http.Header{"Content-Type": {"application/octet-stream"}}
				headers.Set("ETag", `"media-version"`)
				if tc.contentLength != "" {
					headers.Set("Content-Length", tc.contentLength)
				}
				if tc.contentRange != "" {
					headers.Set("Content-Range", tc.contentRange)
				}
				return &http.Response{StatusCode: tc.status, Header: headers, Body: &countedMediaBody{Reader: strings.NewReader("不得读取"), reads: &reads, closed: &closed}, Request: req}, nil
			})}
			req := mediaStabilityRequest(user, info, "")
			req.Method = http.MethodHead
			rec := httptest.NewRecorder()
			s.stream(rec, req)
			if rec.Code != 200 || rec.Header().Get("Content-Length") != tc.wantLength || rec.Header().Get("Content-Range") != "" || rec.Header().Get("ETag") != `"media-version"` {
				t.Fatalf("完整媒体元数据错误: status=%d headers=%v", rec.Code, rec.Header())
			}
			wantRanges := ""
			if tc.status == 206 && tc.wantLength != "" {
				wantRanges = "bytes"
			}
			if rec.Header().Get("Accept-Ranges") != wantRanges {
				t.Fatal("只有有效的部分响应才能推断字节范围能力")
			}
			if reads.Load() != 0 || closed.Load() != 1 || rec.Body.Len() != 0 {
				t.Fatal("预检必须立即关闭上游，不能读取或转发正文")
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("HEAD 不能持久化播放歌曲")
			}
		})
	}
}

func TestMediaHEADFailureNeverReportsSuccess(t *testing.T) {
	for _, mode := range []string{"redirect", "proxy"} {
		for _, failure := range []string{"network", "html", "expired", "range"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				s, user, info := newMediaStabilityServer(t, recoveryScript)
				s.Diagnostics = &diagnostics.Events{}
				raw, _ := json.Marshal(mode)
				if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
					t.Fatal(err)
				}
				var reads, closed atomic.Int32
				s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if failure == "network" {
						return nil, &net.DNSError{Err: "PRIVATE_NETWORK_ERROR", Name: "PRIVATE_HOST", IsNotFound: true}
					}
					status := 200
					if failure == "expired" {
						status = 403
					} else if failure == "range" {
						status = 416
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/html"}}, Body: &countedMediaBody{Reader: strings.NewReader("PRIVATE_ERROR_PAGE"), reads: &reads, closed: &closed}, Request: req}, nil
				})}
				req := mediaStabilityRequest(user, info, "")
				req.Method = http.MethodHead
				req, finish := s.beginClientDiagnostic(req, "stream")
				rec := httptest.NewRecorder()
				s.stream(rec, req)
				finish()
				if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" || rec.Body.Len() != 0 || reads.Load() != 0 {
					t.Fatalf("预检失败不能假报成功或交付未验证地址: status=%d headers=%v", rec.Code, rec.Header())
				}
				events := s.Diagnostics.List()
				last := events[len(events)-1]
				if last.Result != "failed" || last.Status != 502 || last.ProtocolCode == nil || *last.ProtocolCode != ErrGeneric {
					t.Fatalf("预检错误必须保留 HTTP 与协议诊断: %+v", last)
				}
			})
		}
	}
}
