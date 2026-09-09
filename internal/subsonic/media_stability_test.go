package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

const recoveryScript = `
let count = 0
lx.on(lx.EVENT_NAMES.request, ({info}) => { count++; return Promise.resolve('https://cdn.example/' + count) })
lx.send(lx.EVENT_NAMES.inited, {status:true, sources:{wy:{name:'测试',type:'music',actions:['musicUrl'],qualitys:['320k']}}})`

func newMediaStabilityServer(t *testing.T, script string) (*Server, *db.User, *music.Info) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	user, err := database.CreateUser(ctx, "测试用户", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &http.Client{}
	sources := js.NewSourceManager(string(prelude), client, client, log)
	t.Cleanup(sources.UnloadAll)
	if _, err := sources.Load(ctx, 1, 1, script); err != nil {
		t.Fatal(err)
	}
	catalog := music.NewCatalog(database, nil, sources, store, log)
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "temporary", "name": "临时歌曲", "types": []any{map[string]any{"type": "320k"}, map[string]any{"type": "flac"}}})
	catalog.Cache([]*music.Info{info})
	return &Server{DB: database, Catalog: catalog, Settings: store, Log: log, HTTP: client}, user, info
}

func mediaStabilityRequest(user *db.User, info *music.Info, extra string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/rest/stream.view?f=json&id="+info.TrackID()+extra, nil)
	req.Header.Set("Range", "bytes=10-19")
	req.Header.Set("If-Range", `"版本"`)
	return req.WithContext(withUser(req.Context(), user))
}

type trackedMediaBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b *trackedMediaBody) Close() error { b.closed.Add(1); return nil }

func TestProxyRecoveryStatusMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                string
		first, second, gets int
		success             bool
	}{
		{"403后恢复", 403, 206, 2, true},
		{"404后恢复", 404, 200, 2, true},
		{"410后恢复", 410, 206, 2, true},
		{"二次403", 403, 403, 2, false},
		{"二次404", 404, 404, 2, false},
		{"二次410", 410, 410, 2, false},
		{"二次500", 403, 500, 2, false},
		{"首次200", 200, 0, 1, true},
		{"首次206", 206, 0, 1, true},
		{"416不重试", 416, 0, 1, false},
		{"429不重试", 429, 0, 1, false},
		{"500不重试", 500, 0, 1, false},
		{"503不重试", 503, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			var gets, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				n := int(gets.Add(1))
				if req.URL.Path != fmt.Sprintf("/%d", n) {
					t.Errorf("应重新解析不同版本: %s", req.URL.Path)
				}
				if req.Method != http.MethodGet || req.Header.Get("Range") != "bytes=10-19" || req.Header.Get("If-Range") != `"版本"` || req.Header.Get("Referer") != "https://music.163.com/" || req.Header.Get("User-Agent") == "" {
					t.Error("重试必须保持媒体请求头")
				}
				status := tc.first
				if n > 1 {
					status = tc.second
					if closed.Load() != 1 {
						t.Error("刷新之前必须关闭失败响应体")
					}
				}
				headers := http.Header{"Content-Type": []string{"application/octet-stream"}, "ETag": []string{`"版本"`}, "Accept-Ranges": []string{"bytes"}}
				if status == 206 {
					headers.Set("Content-Range", "bytes 10-19/100")
					headers.Set("Content-Length", "10")
				}
				if status == 416 {
					headers.Set("Content-Range", "bytes */100")
				}
				return &http.Response{StatusCode: status, Header: headers, Body: &trackedMediaBody{Reader: strings.NewReader("0123456789"), closed: &closed}, Request: req}, nil
			})}
			recorder := httptest.NewRecorder()
			s.stream(recorder, mediaStabilityRequest(user, info, "&proxy=1"))
			if int(gets.Load()) != tc.gets || int(closed.Load()) != tc.gets {
				t.Fatalf("请求/关闭次数错误: %d/%d", gets.Load(), closed.Load())
			}
			_, err := s.DB.GetTrack(context.Background(), info.TrackID())
			if (err == nil) != tc.success {
				t.Fatalf("落库边界错误: %v", err)
			}
			if tc.success {
				want := tc.first
				if tc.gets == 2 {
					want = tc.second
				}
				if recorder.Code != want || recorder.Body.String() != "0123456789" || recorder.Header().Get("Content-Type") != "audio/mpeg" {
					t.Fatalf("最终媒体响应错误: %d %v %s", recorder.Code, recorder.Header(), recorder.Body.String())
				}
				if want == 206 && recorder.Header().Get("Content-Range") != "bytes 10-19/100" {
					t.Fatal("必须透传 Content-Range")
				}
			} else if tc.first == 416 {
				if recorder.Code != 416 || recorder.Header().Get("Content-Range") != "bytes */100" {
					t.Fatal("416 应按媒体协议透传")
				}
			} else if !strings.Contains(recorder.Body.String(), `"status":"failed"`) {
				t.Fatalf("应保持 Subsonic 错误格式: %s", recorder.Body.String())
			}
		})
	}
}

func TestProxyRefreshFailureAndNetworkFailure(t *testing.T) {
	for _, network := range []bool{false, true} {
		t.Run(fmt.Sprint(network), func(t *testing.T) {
			script := strings.Replace(recoveryScript, "count++; return", "count++; if(count>1) return Promise.reject(new Error('刷新失败')); return", 1)
			s, user, info := newMediaStabilityServer(t, script)
			var gets, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gets.Add(1)
				if network {
					return nil, errors.New("包含签名的敏感地址")
				}
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: &trackedMediaBody{Reader: strings.NewReader("失败"), closed: &closed}, Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, "&proxy=1"))
			if gets.Load() != 1 {
				t.Fatal("解析失败/网络失败不得继续 GET")
			}
			if !network && closed.Load() != 1 {
				t.Fatal("首次失败响应必须关闭")
			}
			if strings.Contains(rec.Body.String(), "敏感地址") {
				t.Fatal("不能透传包含签名的网络错误")
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("失败不得落库")
			}
		})
	}
}

type brokenMediaReader struct{}

func (brokenMediaReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestProxyTransferFailureAndCancellation(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var gets, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gets.Add(1)
				if cancelRequest {
					cancel()
				}
				return &http.Response{StatusCode: 206, Header: make(http.Header), Body: &trackedMediaBody{Reader: io.MultiReader(strings.NewReader("已发送"), brokenMediaReader{}), closed: &closed}, Request: req}, nil
			})}
			req := mediaStabilityRequest(user, info, "&proxy=1")
			req = req.WithContext(withUser(ctx, user))
			rec := httptest.NewRecorder()
			s.stream(rec, req)
			if gets.Load() != 1 || closed.Load() != 1 {
				t.Fatal("取消或传输中断不得重试，必须关闭响应")
			}
			if !cancelRequest && (rec.Code != 206 || rec.Body.String() != "已发送") {
				t.Fatal("提交媒体响应后不得追加协议错误")
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("传输失败不得落库")
			}
		})
	}
}

func TestRecoveryDownloadAndRedirectBoundaries(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint(proxy), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			if proxy {
				if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)}); err != nil {
					t.Fatal(err)
				}
			}
			var gets atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				n := gets.Add(1)
				status := 200
				if n == 1 {
					status = 403
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("音频")), Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			s.download(rec, mediaStabilityRequest(user, info, ""))
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("下载成功也不应持久化")
			}
			if proxy {
				if gets.Load() != 2 || rec.Body.String() != "音频" {
					t.Fatal("下载代理应支持限次恢复")
				}
			} else {
				if gets.Load() != 2 || rec.Code != 302 || rec.Header().Get("Location") != "https://cdn.example/2" {
					t.Fatal("首次302必须校验，过期刷新后的新直链也要复验")
				}
				s.stream(httptest.NewRecorder(), mediaStabilityRequest(user, info, ""))
				if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err != nil {
					t.Fatal("302 stream 应保持原落库行为")
				}
			}
		})
	}
}

func TestConcurrentProxyRecoverySharesOneRefresh(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	if _, err := s.Catalog.ResolveURL(context.Background(), info, "320k"); err != nil {
		t.Fatal(err)
	}
	var oldGets, newGets, closed atomic.Int32
	allOld := make(chan struct{})
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		switch req.URL.Path {
		case "/1":
			if oldGets.Add(1) == 20 {
				close(allOld)
			}
			select {
			case <-allOld:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			status = http.StatusForbidden
		case "/2":
			newGets.Add(1)
		default:
			t.Errorf("同一旧地址不应触发多轮刷新: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: &trackedMediaBody{Reader: strings.NewReader("音频"), closed: &closed}, Request: req}, nil
	})}
	results := make(chan *httptest.ResponseRecorder, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 20 {
		go func() {
			req := mediaStabilityRequest(user, info, "&proxy=1")
			req = req.WithContext(withUser(ctx, user))
			rec := httptest.NewRecorder()
			s.stream(rec, req)
			results <- rec
		}()
	}
	for range 20 {
		rec := <-results
		if rec.Code != http.StatusOK || rec.Body.String() != "音频" {
			t.Fatalf("并发恢复失败: %d %s", rec.Code, rec.Body.String())
		}
	}
	if oldGets.Load() != 20 || newGets.Load() != 20 || closed.Load() != 40 {
		t.Fatalf("每个播放请求最多两次 GET，且只使用一个新解析版本: old=%d new=%d closed=%d", oldGets.Load(), newGets.Load(), closed.Load())
	}
}

func TestRecoveryUsesFinalQuality(t *testing.T) {
	script := `
let count=0
lx.on(lx.EVENT_NAMES.request,({info})=>{
  count++
  if(count>1 && info.type==='flac') return Promise.reject(new Error('该音质暂不可用'))
  return Promise.resolve('https://cdn.example/'+info.type)
})
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{name:'测试',type:'music',actions:['musicUrl'],qualitys:['flac','320k']}}})`
	s, user, info := newMediaStabilityServer(t, script)
	var gets atomic.Int32
	s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := gets.Add(1)
		status := 200
		if n == 1 {
			status = 403
			if req.URL.Path != "/flac" {
				t.Error("首次应请求 flac")
			}
		} else if req.URL.Path != "/320k" {
			t.Error("恢复应沿用音质降级")
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("音频")), Request: req}, nil
	})}
	rec := httptest.NewRecorder()
	s.stream(rec, mediaStabilityRequest(user, info, "&proxy=1&quality=flac"))
	if gets.Load() != 2 || rec.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatal("兜底类型必须使用最终音质")
	}
}
