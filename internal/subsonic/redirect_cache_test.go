package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type probeOnlyBody struct {
	reads, closed atomic.Int32
}

func (b *probeOnlyBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (b *probeOnlyBody) Close() error             { b.closed.Add(1); return nil }

func TestRedirectCachedURLStatusMatrix(t *testing.T) {
	for _, mode := range []string{"redirect", "force_redirect"} {
		for _, endpoint := range []string{"stream", "download"} {
			for _, status := range []int{200, 206, 403, 404, 410, 401, 405, 416, 429, 500, 503} {
				t.Run(fmt.Sprintf("%s/%s/%d", mode, endpoint, status), func(t *testing.T) {
					s, user, info := newMediaStabilityServer(t, recoveryScript)
					if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(fmt.Sprintf("%q", mode))}); err != nil {
						t.Fatal(err)
					}
					if _, err := s.Catalog.ResolveURL(context.Background(), info, "320k"); err != nil {
						t.Fatal(err)
					}
					body := &probeOnlyBody{}
					var checks atomic.Int32
					s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						n := checks.Add(1)
						if req.Method != http.MethodGet || req.URL.Path != fmt.Sprintf("/%d", n) || req.Header.Get("Range") != "bytes=0-0" || req.Header.Get("If-Range") != "" || req.Header.Get("Accept-Encoding") != "identity" {
							t.Errorf("缓存及刷新直链都只校验最小 Range: %s %v", req.URL.Path, req.Header)
						}
						if req.Header.Get("Referer") != "" || req.Header.Get("User-Agent") == "" || req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" {
							t.Error("302 由客户端自行访问：校验不带平台 Referer，也不能继承客户端凭据")
						}
						deadline, ok := req.Context().Deadline()
						if !ok || time.Until(deadline) > 3*time.Second {
							t.Error("校验必须限制为 3 秒")
						}
						return &http.Response{StatusCode: status, Header: http.Header{"Content-Length": {"1000000000"}}, Body: body, Request: req}, nil
					})}
					extra := ""
					if mode == "force_redirect" {
						extra = "&proxy=1"
					}
					req := mediaStabilityRequest(user, info, extra)
					req.Header.Set("Authorization", "客户端凭据")
					req.Header.Set("Cookie", "session=客户端会话")
					rec := httptest.NewRecorder()
					if endpoint == "stream" {
						s.stream(rec, req)
					} else {
						s.download(rec, req)
					}
					success := status == 200 || status == 206
					// 缓存地址失败后向音源重新取链；新地址被过期类状态拒绝时再取一次；限流期间不再请求。
					wantChecks := int32(2)
					switch status {
					case 200, 206, 429:
						wantChecks = 1
					case 403, 404, 410:
						wantChecks = 3
					}
					if success {
						if rec.Code != 302 || rec.Header().Get("Location") != "https://cdn.example/1" || rec.Header().Get("Cache-Control") != "no-store" {
							t.Fatalf("应交付校验成功的直链: %d %v", rec.Code, rec.Header())
						}
					} else if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
						t.Fatalf("唯一音源明确失败，不能302到未复验或已拒绝的地址: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
					}
					if checks.Load() != wantChecks || body.closed.Load() != wantChecks || body.reads.Load() != 0 {
						t.Fatalf("校验次数错误，或没有立即关闭响应、读取了正文: %d/%d/%d", checks.Load(), body.closed.Load(), body.reads.Load())
					}
					_, storedErr := s.DB.GetTrack(context.Background(), info.TrackID())
					if (storedErr == nil) != (endpoint == "stream" && success) {
						t.Fatal("保持播放与下载的歌曲持久化边界")
					}
				})
			}
		}
	}
}

func TestRedirectProbeUnknownAndRefreshFailure(t *testing.T) {
	for _, failure := range []string{"网络", "超时", "取链失败"} {
		t.Run(failure, func(t *testing.T) {
			script := recoveryScript
			if failure == "取链失败" {
				script = strings.Replace(script, "count++; return", "count++; if(count>1) return Promise.reject(new Error('刷新失败')); return", 1)
			}
			s, user, info := newMediaStabilityServer(t, script)
			if _, err := s.Catalog.ResolveURL(context.Background(), info, "320k"); err != nil {
				t.Fatal(err)
			}
			var checks atomic.Int32
			body := &probeOnlyBody{}
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				checks.Add(1)
				if failure == "超时" {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				if failure == "网络" {
					return nil, errors.New("包含签名的敏感地址")
				}
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: body, Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			started := time.Now()
			s.stream(rec, mediaStabilityRequest(user, info, ""))
			// 缓存地址不确定时重新取链一次；重新取链失败则只有缓存地址的那一次校验。
			wantChecks := int32(2)
			if failure == "取链失败" {
				wantChecks = 1
			}
			if time.Since(started) > 7*time.Second || checks.Load() != wantChecks {
				t.Fatal("校验应及时结束且每个地址只校验一次", checks.Load())
			}
			if failure == "取链失败" {
				if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) || body.closed.Load() != 1 {
					t.Fatalf("刷新失败应返回协议错误: %v %s", rec.Header(), rec.Body.String())
				}
				if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
					t.Fatal("刷新失败不能记录播放")
				}
			} else if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
				t.Fatalf("未取得音频不能继续客户端直连: %d %v", rec.Code, rec.Header())
			}
			if strings.Contains(rec.Body.String(), "敏感地址") {
				t.Fatal("不能透传包含签名的网络错误")
			}
		})
	}
}

func TestRedirectWithoutCacheChecksNewURLs(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"urlCacheTTL": json.RawMessage(`0`)}); err != nil {
		t.Fatal(err)
	}
	s.Catalog.RefreshTTL()
	var checks atomic.Int32
	body := &probeOnlyBody{}
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		checks.Add(1)
		if req.Header.Get("Range") != "bytes=0-0" {
			t.Error("新直链也必须只校验响应头")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		s.stream(rec, mediaStabilityRequest(user, info, ""))
		if rec.Code != 302 || rec.Header().Get("Location") != fmt.Sprintf("https://cdn.example/%d", i) {
			t.Fatalf("不缓存应逐次取链: %d %v", rec.Code, rec.Header())
		}
	}
	if checks.Load() != 2 || body.closed.Load() != 2 || body.reads.Load() != 0 {
		t.Fatal("关闭缓存不能跳过新直链校验，也不能读取正文")
	}
}

func TestRedirectLateProbeCannotInvalidateNewURL(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	old, err := s.Catalog.ResolvePlaybackURL(context.Background(), info, "320k")
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := 200
		if req.URL.Path == "/1" {
			close(started)
			<-release
			status = 403
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: &probeOnlyBody{}, Request: req}, nil
	})}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.stream(rec, mediaStabilityRequest(user, info, ""))
		done <- rec
	}()
	<-started
	fresh, err := s.Catalog.RefreshPlaybackURL(context.Background(), info, old)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if rec.Code != 302 || rec.Header().Get("Location") != fresh.Result.URL || fresh.Result.URL != "https://cdn.example/2" {
		t.Fatalf("迟到的失效校验应复用其他请求的新直链: %d %v", rec.Code, rec.Header())
	}
}

func TestRedirectProbeCancellationDoesNotPersistPlayback(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	if _, err := s.Catalog.ResolveURL(context.Background(), info, "320k"); err != nil {
		t.Fatal(err)
	}
	started, cancelled := make(chan struct{}), make(chan struct{})
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		close(cancelled)
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	rec := httptest.NewRecorder()
	go func() {
		req := mediaStabilityRequest(user, info, "")
		s.stream(rec, req.WithContext(withUser(ctx, user)))
		close(done)
	}()
	<-started
	cancel()
	<-done
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("无人等待时应取消上游校验")
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("取消后不能继续返回直链")
	}
	if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
		t.Fatal("取消的请求不能记录播放")
	}
}
