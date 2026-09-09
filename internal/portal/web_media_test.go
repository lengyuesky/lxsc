package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"lxsc/internal/assets"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/subsonic"
)

type webMediaTransport func(*http.Request) (*http.Response, error)

func (fn webMediaTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func webMediaFixture(t *testing.T) (*portalFixture, *subsonic.Server) {
	t.Helper()
	f := newPortalFixture(t)
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	sources := js.NewSourceManager(string(prelude), client, client, f.service.Log)
	t.Cleanup(sources.UnloadAll)
	script := `let count=0;
lx.on(lx.EVENT_NAMES.request, ({info}) => Promise.resolve('https://media.invalid/'+info.type+'/'+(++count)));
lx.send(lx.EVENT_NAMES.inited, {status:true,sources:{wy:{name:'测试',type:'music',actions:['musicUrl'],qualitys:['128k','320k','flac','flac24bit']}}});`
	if _, err := sources.Load(context.Background(), 1, 1, script); err != nil {
		t.Fatal(err)
	}
	catalog := music.NewCatalog(f.db, nil, sources, f.service.Settings, f.service.Log)
	f.service.Catalog = catalog
	media := &subsonic.Server{DB: f.db, Catalog: catalog, Settings: f.service.Settings, Secret: f.service.Secret, Log: f.service.Log, HTTP: client}
	f.service.Stream = media.ServeWebStream
	f.mux.Handle("/rest/", http.StripPrefix("/rest", media.Routes()))
	return f, media
}

func TestWebStreamCookieAndRedirectContract(t *testing.T) {
	f, media := webMediaFixture(t)
	var calls atomic.Int32
	media.HTTP = &http.Client{Transport: webMediaTransport(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.Method != http.MethodGet || req.Header.Get("Range") != "bytes=0-0" || req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Error("网页直连只允许最小 Range 校验，不能转发用户凭据")
		}
		return &http.Response{StatusCode: 206, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}
	url := f.server.URL + "/stream?id=tr-wy-1"
	if status := requestStatus(t, http.DefaultClient, http.MethodGet, url+"&u=alice&p=pw-alice"); status != 401 {
		t.Fatalf("网页接口只能使用 Cookie，实际状态 %d", status)
	}
	alice := f.client(t, "alice")
	alice.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	seen := map[string]bool{}
	var wantChecks int32
	for _, mode := range []string{"redirect", "force_redirect"} {
		t.Run(mode, func(t *testing.T) {
			raw, err := json.Marshal(mode)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct{ query, quality string }{{"", "320k"}, {"&quality=flac", "flac"}, {"&proxy=1", "320k"}} {
				if seen[tc.quality] {
					wantChecks++
				}
				seen[tc.quality] = true
				resp, err := alice.Get(url + tc.query)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "https://media.invalid/"+tc.quality+"/") || resp.Header.Get("Cache-Control") != "no-store" || calls.Load() != wantChecks {
					t.Fatalf("应遵循直连配置和音质，只校验缓存且不接受 proxy 覆盖: %d %v calls=%d", resp.StatusCode, resp.Header, calls.Load())
				}
			}
		})
	}
	for _, query := range []string{"id=bad", "id=tr-wy", "id=al-wy-1", "id=tr-bad-1", "id=tr-wy-1&quality=master"} {
		if status := requestStatus(t, alice, http.MethodGet, f.server.URL+"/stream?"+query); status != 400 {
			t.Fatalf("无效参数应返回 400: %s %d", query, status)
		}
	}
	if status := requestStatus(t, alice, http.MethodGet, f.server.URL+"/stream?id=tr-wy-missing"); status != 404 {
		t.Fatalf("未知歌曲应返回 404，实际 %d", status)
	}
	resp, err := alice.Get(f.server.URL + "/rest/stream.view?id=tr-wy-1&f=json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"status":"failed"`) || !strings.Contains(string(body), `"code":10`) {
		t.Fatalf("网页登录 Cookie 不能扩大到 Subsonic 认证: %s", body)
	}
	if status, _ := doJSON(t, alice, http.MethodPost, f.server.URL+"/auth/logout", nil); status != 200 {
		t.Fatal("注销失败")
	}
	if status := requestStatus(t, alice, http.MethodGet, url); status != 401 {
		t.Fatal("注销后不能继续请求网页音频")
	}
}

func TestWebProxyMediaStatusesAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, second int
		want, calls   int
		persist       bool
	}{
		{"完整音频", 200, 0, 200, 1, true},
		{"范围音频", 206, 0, 206, 1, true},
		{"越界范围", 416, 0, 416, 1, false},
		{"403 刷新", 403, 206, 206, 2, true},
		{"404 刷新", 404, 200, 200, 2, true},
		{"410 刷新", 410, 206, 206, 2, true},
		{"刷新后仍失败", 403, 403, 502, 2, false},
		{"不重试限流", 429, 0, 502, 1, false},
		{"不重试服务错误", 500, 0, 502, 1, false},
		{"不泄漏网络错误", 0, 0, 502, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, media := webMediaFixture(t)
			if _, err := f.service.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)}); err != nil {
				t.Fatal(err)
			}
			trackID := cacheTemporaryTrack(f, "stream-temporary")
			var calls atomic.Int32
			media.HTTP = &http.Client{Transport: webMediaTransport(func(r *http.Request) (*http.Response, error) {
				n := int(calls.Add(1))
				if r.Header.Get("Range") != "bytes=10-19" || r.Header.Get("If-Range") != `"version"` || r.Header.Get("Referer") != "https://music.163.com/" || r.Header.Get("Cookie") != "" {
					t.Errorf("媒体头应透传，登录 Cookie 不能转发到上游: %v", r.Header)
				}
				if tc.first == 0 {
					return nil, errors.New("网络失败 https://secret.invalid/?token=私密")
				}
				status := tc.first
				if n > 1 {
					status = tc.second
				}
				headers := http.Header{"Content-Type": {"application/octet-stream"}, "Accept-Ranges": {"bytes"}, "ETag": {`"version"`}}
				if status == 206 {
					headers.Set("Content-Range", "bytes 10-19/100")
				}
				if status == 416 {
					headers.Set("Content-Range", "bytes */100")
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader("0123456789")), Request: r}, nil
			})}
			client := f.client(t, "alice")
			req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/stream?id="+trackID, nil)
			req.Header.Set("Range", "bytes=10-19")
			req.Header.Set("If-Range", `"version"`)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.want || int(calls.Load()) != tc.calls {
				t.Fatalf("状态或恢复次数错误: %d calls=%d body=%s", resp.StatusCode, calls.Load(), body)
			}
			if tc.persist && (string(body) != "0123456789" || resp.Header.Get("Content-Type") != "audio/mpeg") {
				t.Fatalf("成功响应应是音频流: %v %s", resp.Header, body)
			}
			if tc.want == 416 && resp.Header.Get("Content-Range") != "bytes */100" {
				t.Fatal("416 必须保留范围头")
			}
			if tc.want == 502 && (!strings.Contains(string(body), `"error"`) || strings.Contains(string(body), "secret.invalid") || strings.Contains(string(body), "subsonic-response")) {
				t.Fatalf("Web 错误格式或脱敏失败: %s", body)
			}
			_, storedErr := f.db.GetTrack(context.Background(), trackID)
			if (storedErr == nil) != tc.persist {
				t.Fatalf("播放元数据持久化边界错误: %v", storedErr)
			}
		})
	}
}
