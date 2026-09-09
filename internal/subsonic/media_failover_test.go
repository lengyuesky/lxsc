package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

func mediaFailoverScript(label string) string {
	return fmt.Sprintf(`// @name 同名测试音源
let count=0;
lx.on(lx.EVENT_NAMES.request, () => Promise.resolve('https://media.invalid/%s/'+(++count)));
lx.send(lx.EVENT_NAMES.inited, {status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`, label)
}

func TestMediaFailoverDifferentScriptIDs(t *testing.T) {
	for _, tc := range []struct {
		name, mode, endpoint string
		cached               bool
	}{
		{"代理播放", "proxy", "stream", false},
		{"首次302", "redirect", "stream", false},
		{"缓存302", "redirect", "stream", true},
		{"强制302", "force_redirect", "stream", false},
		{"代理下载", "proxy", "download", false},
		{"重定向下载", "redirect", "download", false},
		{"网页代理", "proxy", "web", false},
		{"网页重定向", "redirect", "web", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			// 两个脚本故意同名；平台同为 wy，只有真实脚本 ID 不同。
			if _, err := s.Catalog.Sources.Load(context.Background(), 22, 2, mediaFailoverScript("b")); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(tc.mode)
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
				t.Fatal(err)
			}
			if tc.cached {
				if _, err := s.Catalog.ResolvePlaybackURL(context.Background(), info, "320k"); err != nil {
					t.Fatal(err)
				}
			}
			var paths []string
			var reads, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if int(closed.Load()) != len(paths) {
					t.Error("尝试下一链接之前必须关闭前一个失败响应")
				}
				paths = append(paths, req.URL.Path)
				if req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" || req.Header.Get("User-Agent") == "" || req.Header.Get("Referer") != "https://music.163.com/" {
					t.Error("媒体请求必须使用播放UA/Referer且不继承用户凭据")
				}
				if tc.mode == "proxy" {
					if req.Header.Get("Range") != "bytes=10-19" || req.Header.Get("If-Range") != `"版本"` {
						t.Error("换源不能丢失Range/If-Range")
					}
				} else if req.Header.Get("Range") != "bytes=0-0" || req.Header.Get("If-Range") != "" || req.Header.Get("Accept-Encoding") != "identity" {
					t.Error("302换源只允许最小Range校验")
				}
				status := http.StatusForbidden
				if strings.HasPrefix(req.URL.Path, "/b/") {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: &countedMediaBody{Reader: strings.NewReader("备选音频"), reads: &reads, closed: &closed}, Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			extra := ""
			if tc.mode == "force_redirect" {
				extra = "&proxy=1"
			}
			req := mediaStabilityRequest(user, info, extra)
			req.Header.Set("Cookie", "session=合成会话")
			req.Header.Set("Authorization", "合成凭据")
			switch tc.endpoint {
			case "download":
				s.download(rec, req)
			case "web":
				s.ServeWebStream(rec, req, user)
			default:
				s.stream(rec, req)
			}
			if want := []string{"/a/1", "/a/2", "/b/1"}; !reflect.DeepEqual(paths, want) {
				t.Fatalf("A 刷新后仍为403必须真正访问不同ID的B：请求=%v，期望=%v，响应=%d", paths, want, rec.Code)
			}
			if tc.mode == "proxy" {
				if rec.Code != http.StatusOK || rec.Body.String() != "备选音频" {
					t.Fatalf("代理必须使用B：%d %s", rec.Code, rec.Body.String())
				}
			} else if rec.Code != http.StatusFound || rec.Header().Get("Location") != "https://media.invalid/b/1" {
				t.Fatalf("302必须交付已校验的B：%d %v", rec.Code, rec.Header())
			}
			if closed.Load() != 3 || (tc.mode != "proxy" && reads.Load() != 0) {
				t.Fatalf("响应关闭或仅取头边界错误：关闭=%d，读取=%d", closed.Load(), reads.Load())
			}
			_, storedErr := s.DB.GetTrack(context.Background(), info.TrackID())
			if (storedErr == nil) != (tc.endpoint != "download") {
				t.Fatal("只有成功播放可以持久化，下载不可以")
			}
			cached, err := s.Catalog.ResolvePlaybackURL(context.Background(), info, "320k")
			if err != nil || !cached.Cached || cached.Result.URL != "https://media.invalid/b/1" {
				t.Fatalf("后续请求应复用成功切换的B：%+v %v", cached, err)
			}
		})
	}
}

type countedMediaBody struct {
	io.Reader
	reads, closed *atomic.Int32
}

func (b *countedMediaBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.Reader.Read(p)
}
func (b *countedMediaBody) Close() error { b.closed.Add(1); return nil }

func addMediaFallback(t *testing.T, s *Server, id int64, priority int, label string) {
	t.Helper()
	if _, err := s.Catalog.Sources.Load(context.Background(), id, priority, mediaFailoverScript(label)); err != nil {
		t.Fatal(err)
	}
}

func TestMediaFailoverRecoverableFailures(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		for _, first := range []int{0, 204, 400, 401, 403, 404, 405, 408, 410, 429, 500, 501, 502, 503, 504} {
			t.Run(fmt.Sprintf("代理=%t/状态=%d", proxy, first), func(t *testing.T) {
				s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
				addMediaFallback(t, s, 22, 2, "b")
				var paths []string
				s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					paths = append(paths, req.URL.Path)
					status := 200
					if strings.HasPrefix(req.URL.Path, "/a/") {
						if first == 0 {
							return nil, &net.DNSError{Err: "合成超时", Name: "不可泄露的签名地址", IsTimeout: true}
						}
						status = first
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("音频")), Request: req}, nil
				})}
				extra := ""
				if proxy {
					extra = "&proxy=1"
				}
				rec := httptest.NewRecorder()
				s.stream(rec, mediaStabilityRequest(user, info, extra))
				want := []string{"/a/1", "/b/1"}
				if expiredMediaStatus(first) {
					want = []string{"/a/1", "/a/2", "/b/1"}
				}
				if !reflect.DeepEqual(paths, want) || (proxy && rec.Body.String() != "音频") || (!proxy && rec.Header().Get("Location") != "https://media.invalid/b/1") {
					t.Fatalf("可恢复失败必须跳过坏源：请求=%v，响应=%d %v", paths, rec.Code, rec.Header())
				}
			})
		}
	}
}

func TestMediaFailoverInvalidURL(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint(proxy), func(t *testing.T) {
			script := strings.ReplaceAll(mediaFailoverScript("a"), "https://media.invalid", "file:///不可访问的合成路径")
			s, user, info := newMediaStabilityServer(t, script)
			addMediaFallback(t, s, 22, 2, "b")
			calls := 0
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Scheme != "https" || req.URL.Path != "/b/1" {
					t.Error("无效协议不能交给媒体传输")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("音频")), Request: req}, nil
			})}
			extra := ""
			if proxy {
				extra = "&proxy=1"
			}
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, extra))
			if calls != 1 || (proxy && rec.Body.String() != "音频") || (!proxy && rec.Header().Get("Location") != "https://media.invalid/b/1") {
				t.Fatal("无效链接必须跳过原脚本并选择B")
			}
		})
	}
}

func TestMediaFailoverAllRejectedIsBounded(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint(proxy), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			for i, label := range []string{"b", "c", "d"} {
				addMediaFallback(t, s, int64(i+2), i+2, label)
			}
			var paths []string
			var reads, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				paths = append(paths, req.URL.Path)
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: &countedMediaBody{Reader: strings.NewReader("错误正文"), reads: &reads, closed: &closed}, Request: req}, nil
			})}
			extra := ""
			if proxy {
				extra = "&proxy=1"
			}
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, extra))
			want := []string{"/a/1", "/a/2", "/b/1", "/b/2", "/c/1", "/c/2"}
			if !reflect.DeepEqual(paths, want) || reads.Load() != 0 || closed.Load() != 6 {
				t.Fatalf("最多三个不同脚本、六次媒体请求，不读取失败正文：%v 读取=%d 关闭=%d", paths, reads.Load(), closed.Load())
			}
			if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
				t.Fatal("全部明确失败必须返回协议错误")
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("全部失败不能记录播放")
			}
		})
	}
}

func TestRedirectFailoverUnknownOpportunity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a, b     int
		location string
	}{
		{"不确定后验证成功", 0, 200, "b/1"},
		{"全部网络不确定", 0, 0, "a/1"},
		{"明确拒绝后网络不确定", 403, 0, "b/1"},
		{"网络不确定后明确拒绝", 0, 403, "a/1"},
		{"全部明确拒绝", 500, 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			addMediaFallback(t, s, 22, 2, "b")
			s.Diagnostics = &diagnostics.Events{}
			var log strings.Builder
			s.Log = slog.New(slog.NewTextHandler(&log, nil))
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status := tc.a
				if strings.HasPrefix(req.URL.Path, "/b/") {
					status = tc.b
				}
				if status == 0 {
					return nil, errors.New("敏感签名 https://private.invalid/?token=不能泄露")
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: &probeOnlyBody{}, Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, ""))
			if tc.location != "" {
				if rec.Code != 302 || rec.Header().Get("Location") != "https://media.invalid/"+tc.location {
					t.Fatalf("只能把未被明确拒绝的直链交给客户端：%d %v", rec.Code, rec.Header())
				}
			} else if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
				t.Fatal("HTTP明确失败不能作为网络不确定继续302")
			}
			events, _ := json.Marshal(s.Diagnostics.List())
			for _, secret := range []string{"敏感签名", "不能泄露", "private.invalid"} {
				if strings.Contains(string(events)+rec.Body.String()+log.String(), secret) {
					t.Fatal("不能泄漏错误原文", secret)
				}
			}
			if strings.Contains(string(events), "同名测试音源") {
				t.Fatal("不能把脚本身份写入结构化诊断")
			}
			stages := map[string]bool{}
			for _, event := range s.Diagnostics.List() {
				stages[event.Stage] = true
			}
			if !stages["url_check"] || !stages["source_fallback"] {
				t.Fatal("新直链校验与请求内换源必须使用固定诊断阶段")
			}
		})
	}
}

func TestMediaFailoverTerminalBoundaries(t *testing.T) {
	for _, scenario := range []string{"预先取消", "代理取消", "302取消", "代理416", "302的416", "copy中断"} {
		t.Run(scenario, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			addMediaFallback(t, s, 22, 2, "b")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var gets, closed atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gets.Add(1)
				if strings.HasSuffix(scenario, "取消") {
					cancel()
					return nil, req.Context().Err()
				}
				status := 416
				var reader io.Reader = strings.NewReader("")
				if scenario == "copy中断" {
					status = 206
					reader = io.MultiReader(strings.NewReader("已提交音频"), brokenMediaReader{})
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Range": {"bytes */100"}}, Body: &trackedMediaBody{Reader: reader, closed: &closed}, Request: req}, nil
			})}
			if scenario == "预先取消" {
				cancel()
			}
			extra := "&proxy=1"
			if strings.HasPrefix(scenario, "302") {
				extra = ""
			}
			req := mediaStabilityRequest(user, info, extra).WithContext(withUser(ctx, user))
			rec := httptest.NewRecorder()
			s.stream(rec, req)
			wantGets := int32(1)
			if scenario == "预先取消" {
				wantGets = 0
			}
			if gets.Load() != wantGets || rec.Header().Get("Location") != "" {
				t.Fatalf("终止状态不能访问B或继续直连：%d %v", gets.Load(), rec.Header())
			}
			if scenario == "代理416" && (rec.Code != 416 || rec.Header().Get("Content-Range") != "bytes */100") {
				t.Fatal("416仍应按媒体协议透传")
			}
			if scenario == "copy中断" && (rec.Code != 206 || rec.Body.String() != "已提交音频") {
				t.Fatal("copy开始后不能重播或追加协议错误")
			}
			if !strings.HasSuffix(scenario, "取消") && closed.Load() != 1 {
				t.Fatal("终止响应也必须关闭正文")
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("取消、416或copy失败不能记录播放")
			}
		})
	}
}

func TestMediaHeaderTimeoutTriesDifferentSource(t *testing.T) {
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint(proxy), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			addMediaFallback(t, s, 22, 2, "b")
			var paths []string
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				paths = append(paths, req.URL.Path)
				if strings.HasPrefix(req.URL.Path, "/a/") {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("音频")), Request: req}, nil
			})}
			extra, limit := "", music.URLCheckTimeout
			if proxy {
				extra, limit = "&proxy=1", mediaHeaderTimeout
			}
			started := time.Now()
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, extra))
			elapsed := time.Since(started)
			if elapsed < limit-time.Second || elapsed > limit+2*time.Second || !reflect.DeepEqual(paths, []string{"/a/1", "/b/1"}) {
				t.Fatalf("头超时必须及时取消A并访问B：耗时=%v，请求=%v", elapsed, paths)
			}
		})
	}
}

func TestMediaRecoveryTotalBudget(t *testing.T) {
	if mediaRecoveryTimeout != 45*time.Second || maxMediaSources != 3 || maxMediaAttempts != 6 {
		t.Fatal("恢复上限必须固定且与文档一致")
	}
	for _, proxy := range []bool{false, true} {
		t.Run(fmt.Sprint(proxy), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("a"))
			addMediaFallback(t, s, 22, 2, "b")
			var gets atomic.Int32
			cancelled := make(chan struct{})
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				gets.Add(1)
				<-req.Context().Done()
				close(cancelled)
				return nil, req.Context().Err()
			})}
			// 缩短同一个预算上下文，验证头请求也受总预算约束，而不是每轮重置45秒。
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			started := time.Now()
			resolution, resp, err := s.prepareMedia(ctx, mediaStabilityRequest(user, info, ""), info, "320k", proxy)
			if time.Since(started) > time.Second || gets.Load() != 1 || resp != nil {
				t.Fatal("预算结束后不能继续尝试其他源")
			}
			if proxy && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("代理应返回预算超时：%v", err)
			}
			if !proxy && (err != nil || resolution.Result.URL != "https://media.invalid/a/1") {
				t.Fatal("仅网络不确定时可保留客户端直连机会")
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("无人等待后上游必须被取消")
			}
		})
	}
}

func TestMediaProxyBodyOutlivesRecoveryBudget(t *testing.T) {
	for _, cancelParent := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelParent), func(t *testing.T) {
			release := make(chan struct{})
			cancelled := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "audio/mpeg")
				_, _ = io.WriteString(w, "首段")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, "尾段")
				case <-r.Context().Done():
					cancelled <- struct{}{}
				}
			}))
			defer upstream.Close()
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			script := strings.ReplaceAll(mediaFailoverScript("a"), "https://media.invalid", upstream.URL)
			s, user, info := newMediaStabilityServer(t, script)
			s.HTTP = upstream.Client()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			budget, stop := context.WithTimeout(parent, 150*time.Millisecond)
			defer stop()
			req := mediaStabilityRequest(user, info, "&proxy=1").WithContext(withUser(parent, user))
			resolution, resp, err := s.prepareMedia(budget, req, info, "320k", true)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			<-budget.Done()
			if resp.Request.Context().Err() != nil {
				t.Fatal("响应头成功后，整首音频不能随恢复预算截止被取消")
			}
			if cancelParent {
				cancel()
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("解除预算后仍必须传播父请求取消")
				}
				return
			}
			close(release)
			released = true
			rec := httptest.NewRecorder()
			if !s.proxyStream(rec, req, info, resolution, resp) || rec.Body.String() != "首段尾段" {
				t.Fatal("超过准备预算的音频必须完整传输")
			}
		})
	}
}

func TestRedirectUnknownCandidatesRespectLaterRejections(t *testing.T) {
	for _, firstUnknown := range []bool{false, true} {
		t.Run(fmt.Sprint(firstUnknown), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("same"))
			second := "same"
			if firstUnknown {
				second = "other"
			}
			addMediaFallback(t, s, 22, 2, second)
			addMediaFallback(t, s, 33, 3, "same")
			var calls atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				n := calls.Add(1)
				if (n <= 2) == firstUnknown {
					return nil, errors.New("合成网络错误")
				}
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: &probeOnlyBody{}, Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, ""))
			if firstUnknown {
				if rec.Code != 302 || rec.Header().Get("Location") != "https://media.invalid/other/1" {
					t.Fatal("首个不确定地址后来被拒绝，仍须保留其他未被拒绝的候选")
				}
			} else if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
				t.Fatal("已明确拒绝的地址不能因后续网络错误重新进入不确定备选")
			}
			if calls.Load() != 4 {
				t.Fatalf("恢复次数错误：%d", calls.Load())
			}
		})
	}
}

func TestRedirectFailoverRejectsPreviouslyUnknownURL(t *testing.T) {
	for _, status := range []int{403, 404, 410, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, mediaFailoverScript("same"))
			// 不同脚本返回相同地址，后续明确拒绝必须覆盖先前网络不确定的判断。
			addMediaFallback(t, s, 22, 2, "same")
			var calls atomic.Int32
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return nil, errors.New("合成网络错误")
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: &probeOnlyBody{}, Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, ""))
			if rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
				t.Fatalf("已明确拒绝的同一URL不能作为不确定备选返回302：%d %v", rec.Code, rec.Header())
			}
			if _, err := s.DB.GetTrack(context.Background(), info.TrackID()); err == nil {
				t.Fatal("全部明确失败不能记录播放")
			}
		})
	}
}

func TestMediaFailoverRefreshParsingAndFinalQuality(t *testing.T) {
	for _, parseFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(parseFailure), func(t *testing.T) {
			script := strings.ReplaceAll(mediaFailoverScript("a"), "['320k']", "['flac']")
			if parseFailure {
				script = strings.Replace(script, "() => Promise.resolve", "() => count > 0 ? Promise.reject(new Error('刷新失败')) : Promise.resolve", 1)
			}
			s, user, info := newMediaStabilityServer(t, script)
			addMediaFallback(t, s, 22, 2, "b")
			var paths []string
			s.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				paths = append(paths, req.URL.Path)
				status := 403
				if strings.HasPrefix(req.URL.Path, "/b/") {
					status = 200
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(strings.NewReader("音频")), Request: req}, nil
			})}
			rec := httptest.NewRecorder()
			s.stream(rec, mediaStabilityRequest(user, info, "&proxy=1&quality=flac"))
			wantGets := 3
			if parseFailure {
				wantGets = 2
			}
			if len(paths) != wantGets || paths[len(paths)-1] != "/b/1" || rec.Body.String() != "音频" || rec.Header().Get("Content-Type") != "audio/mpeg" {
				t.Fatalf("刷新解析失败也应切源，最终Content-Type必须使用B降级后的音质：%v %v", paths, rec.Header())
			}
		})
	}
}
