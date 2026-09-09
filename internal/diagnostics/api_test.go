package diagnostics

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/assets"
	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

type fixture struct {
	databasePath         string
	s                    *Server
	router               http.Handler
	admin, normal        *db.User
	cookie, normalCookie *http.Cookie
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	admin, err := d.CreateUser(context.Background(), "合成管理员", "", true, "320k")
	if err != nil {
		t.Fatal(err)
	}
	normal, err := d.CreateUser(context.Background(), "合成用户", "", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	st, err := settings.New(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	auth := &webauth.Manager{DB: d}
	catalog := music.NewCatalog(d, nil, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s := New(d, auth, catalog, nil, st, "test-version", time.Now())
	t.Cleanup(s.Tokens.Close)
	cookie := func(u *db.User) *http.Cookie {
		w := httptest.NewRecorder()
		auth.Start(w, httptest.NewRequest("GET", "http://debug.test/", nil), u)
		return w.Result().Cookies()[0]
	}
	r := chi.NewRouter()
	r.Mount("/api/admin/debug-tokens", s.ManagementRoutes())
	r.Mount("/api/debug", s.Routes())
	return &fixture{databasePath, s, r, admin, normal, cookie(admin), cookie(normal)}
}
func (f *fixture) call(method, path, body, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://debug.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://debug.test")
	r.Header.Set("X-LXSC-Debug-Management", "1")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, r)
	return w
}
func (f *fixture) create(t *testing.T, probe bool) (TokenView, string) {
	t.Helper()
	v, key, err := f.s.Tokens.Create(f.admin.ID, 900, probe)
	if err != nil {
		t.Fatal(err)
	}
	return v, key
}
func TestManagementAuthTTLAndStrictBody(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		cookie *http.Cookie
		code   int
	}{{nil, 401}, {f.normalCookie, 403}, {f.cookie, 201}} {
		w := f.call("POST", "/api/admin/debug-tokens", "{}", "", tc.cookie)
		if w.Code != tc.code {
			t.Fatalf("%d: %s", w.Code, w.Body)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal(w.Header())
		}
	}
	for _, body := range []string{`{"ttlSeconds":0}`, `{"ttlSeconds":-1}`, `{"ttlSeconds":86401}`, `{"ttlSeconds":9223372036854775808}`, `{"ttlSeconds":900000000000000000000}`, `{"ttlSeconds":900.0}`, `{"ttlSeconds":null}`, `{"ttlSeconds":"900"}`, `{"ttlSeconds":900,"ttlSeconds":900}`, `{"unknown":true}`, `null`, `[]`, `{} {}`, `{"scopes":[]}`, `{"scopes":["probe"]}`, `{"scopes":["read","read"]}`, `{"scopes":["admin"]}`, `{"scopes":null}`, `{"x":"` + strings.Repeat("x", 3000) + `"}`} {
		if w := f.call("POST", "/api/admin/debug-tokens", body, "", f.cookie); w.Code != 400 {
			t.Fatalf("body=%s: %d %s", body, w.Code, w.Body)
		}
	}
	for _, ttl := range []string{"900", "3600", "21600", "86400"} {
		w := f.call("POST", "/api/admin/debug-tokens", `{"ttlSeconds":`+ttl+`,"scopes":["read","probe"]}`, "", f.cookie)
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body)
		}
	}
	w := f.call("POST", "/api/admin/debug-tokens", "{}", "", f.cookie)
	var created struct {
		Token      string    `json:"token"`
		Credential TokenView `json:"credential"`
	}
	if json.Unmarshal(w.Body.Bytes(), &created) != nil || len(created.Token) != 49 {
		t.Fatal(w.Body)
	}
	list := f.call("GET", "/api/admin/debug-tokens", "", "", f.cookie)
	if strings.Contains(list.Body.String(), created.Token) || strings.Contains(list.Body.String(), "digest") || strings.Contains(list.Body.String(), "合成") {
		t.Fatal("列表泄露敏感信息")
	}
	if w := f.call("DELETE", "/api/admin/debug-tokens/"+created.Credential.ID, "", "", f.cookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.call("GET", "/api/debug/status", "", created.Token, nil); w.Code != 401 {
		t.Fatal(w.Code, w.Body)
	}
}
func TestManagementCSRF(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name, origin, header, site, forwarded string
		tls                                   bool
		want                                  int
	}{
		{"同源", "http://debug.test", "1", "same-origin", "", false, 201},
		{"反代HTTPS", "https://debug.test", "1", "same-origin", "", false, 201},
		{"直接TLS", "https://debug.test", "1", "same-origin", "", true, 201},
		{"直接TLS降级", "http://debug.test", "1", "same-origin", "", true, 403},
		{"缺Origin", "", "1", "", "", false, 403}, {"null", "null", "1", "", "", false, 403},
		{"host", "http://evil.test", "1", "", "", false, 403}, {"端口", "http://debug.test:80", "1", "", "", false, 403},
		{"path", "http://debug.test/", "1", "", "", false, 403}, {"query", "http://debug.test?", "1", "", "", false, 403},
		{"fragment", "http://debug.test#", "1", "", "", false, 403}, {"userinfo", "http://user@debug.test", "1", "", "", false, 403},
		{"坏端口", "http://debug.test:bad", "1", "", "", false, 403}, {"缺自定义头", "http://debug.test", "", "", "", false, 403},
		{"cross-site", "http://debug.test", "1", "cross-site", "", false, 403}, {"same-site", "http://debug.test", "1", "same-site", "", false, 403},
		{"伪造转发头", "http://evil.test", "1", "", "evil.test", false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, method := range []string{"POST", "DELETE"} {
				path := "/api/admin/debug-tokens"
				body := "{}"
				want := tc.want
				if method == "DELETE" {
					v, _ := f.create(t, false)
					path += "/" + v.ID
					body = ""
					if want == 201 {
						want = 200
					}
					defer f.s.Tokens.Revoke(v.ID)
				}
				r := httptest.NewRequest(method, "http://debug.test"+path, strings.NewReader(body))
				r.AddCookie(f.cookie)
				r.Header.Set("Content-Type", "application/json")
				if tc.origin != "" {
					r.Header.Set("Origin", tc.origin)
				}
				r.Header.Set("X-LXSC-Debug-Management", tc.header)
				r.Header.Set("Sec-Fetch-Site", tc.site)
				r.Header.Set("X-Forwarded-Host", tc.forwarded)
				r.Header.Set("X-Forwarded-Proto", "https")
				if tc.tls {
					r.TLS = &tls.ConnectionState{}
				}
				w := httptest.NewRecorder()
				f.router.ServeHTTP(w, r)
				if w.Code != want {
					t.Fatalf("%s: %d %s", method, w.Code, w.Body)
				}
			}
		})
	}
}
func TestBearerOnlyScopeOwnerAndExpiry(t *testing.T) {
	f := newFixture(t)
	v, key := f.create(t, false)
	for _, path := range []string{"/api/debug/status?token=" + key, "/api/debug/events?apiKey=" + key} {
		if w := f.call("GET", path, "", "", nil); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	for _, cookie := range []*http.Cookie{nil, f.cookie, f.normalCookie} {
		if w := f.call("GET", "/api/debug/status", "", "", cookie); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	if w := f.call("GET", "/api/admin/debug-tokens", "", key, nil); w.Code != 401 {
		t.Fatal("调试key不能管理", w.Code)
	}
	if w := f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/api/debug/status", "/api/debug/events"} {
		if w := f.call("GET", path, "", key, nil); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if err := f.s.DB.UpdateUser(context.Background(), f.admin.ID, f.admin.Name, "", false, "320k"); err != nil {
		t.Fatal(err)
	}
	if w := f.call("GET", "/api/debug/status", "", key, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if err := f.s.DB.UpdateUser(context.Background(), f.admin.ID, f.admin.Name, "", true, "320k"); err != nil {
		t.Fatal(err)
	}
	if w := f.call("GET", "/api/debug/status", "", key, nil); w.Code != 401 {
		t.Fatal("升权不恢复旧key")
	}
	v, key = f.create(t, false)
	f.s.Tokens.mu.Lock()
	f.s.Tokens.entries[v.ID].ExpiresAt = time.Now()
	f.s.Tokens.mu.Unlock()
	if w := f.call("GET", "/api/debug/status", "", key, nil); w.Code != 401 {
		t.Fatal("绝对到期必须拒绝")
	}
	_, key = f.create(t, false)
	f.s.Tokens.Close()
	if w := f.call("GET", "/api/debug/status", "", key, nil); w.Code != 401 {
		t.Fatal("重启必须失效")
	}
}
func TestRateLimitsCapacityAndAuditBound(t *testing.T) {
	f := newFixture(t)
	_, key := f.create(t, false)
	for i := 0; i < callsPerMinute; i++ {
		if w := f.call("GET", "/api/debug/status", "", key, nil); w.Code != 200 {
			t.Fatal(i, w.Code)
		}
	}
	w := f.call("GET", "/api/debug/status", "", key, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code, w.Header())
	}
	for i := 1; i < maxActiveTokens; i++ {
		f.create(t, false)
	}
	if _, _, err := f.s.Tokens.Create(f.admin.ID, 900, false); err == nil {
		t.Fatal("未限制活跃凭据数量")
	}
	views, _ := f.s.Tokens.List()
	for _, v := range views {
		f.s.Tokens.Revoke(v.ID)
	}
	for i := 0; i < 160; i++ {
		v, _ := f.create(t, false)
		f.s.Tokens.Revoke(v.ID)
	}
	views, audit := f.s.Tokens.List()
	if len(views) > maxTokens || len(audit) != 128 {
		t.Fatal(len(views), len(audit))
	}
	_, key = f.create(t, false)
	for i := 0; i < cap(f.s.concurrent); i++ {
		f.s.concurrent <- struct{}{}
	}
	w = f.call("GET", "/api/debug/status", "", key, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal(w.Code)
	}
	for i := 0; i < cap(f.s.concurrent); i++ {
		<-f.s.concurrent
	}
	_, key = f.create(t, true)
	for i := 0; i < cap(f.s.probes); i++ {
		f.s.probes <- struct{}{}
	}
	w = f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil)
	if w.Code != 429 {
		t.Fatal(w.Code)
	}
	for i := 0; i < cap(f.s.probes); i++ {
		<-f.s.probes
	}
}
func TestConcurrentAcquireRevoke(t *testing.T) {
	f := newFixture(t)
	v, key := f.create(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.s.Tokens.acquire(key); f.s.Tokens.List(); f.s.Tokens.Revoke(v.ID) }()
	}
	wg.Wait()
	if _, _, code := f.s.Tokens.acquire(key); code != 401 {
		t.Fatal(code)
	}
}

// 音源、歌曲、数据库均为合成数据；安全传输默认仍拒绝本地音源地址。
func setupProbe(t *testing.T, f *fixture) {
	t.Helper()
	script := `lx.on(lx.EVENT_NAMES.request,()=>Promise.resolve('https://media.example/audio?signature=NEVER_EXPORT'));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{name:'秘密音源名',type:'music',actions:['musicUrl'],qualitys:['320k']}}});`
	setupProbeScript(t, f, script)
}
func setupProbeScript(t *testing.T, f *fixture, script string) {
	t.Helper()
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sources := js.NewSourceManager(string(prelude), &http.Client{}, &http.Client{}, log)
	t.Cleanup(sources.UnloadAll)

	if _, err := sources.Load(context.Background(), 1, 1, script); err != nil {
		t.Fatal(err)
	}
	f.s.Catalog = music.NewCatalog(f.s.DB, nil, sources, f.s.Settings, log)
	info, err := music.ParseInfo([]byte(`{"source":"wy","songmid":"1","name":"秘密歌曲名","singer":"秘密歌手","types":[{"type":"320k"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	f.s.Catalog.Cache([]*music.Info{info})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type unreadBody struct {
	t      *testing.T
	closed bool
}

func (b *unreadBody) Read([]byte) (int, error) {
	b.t.Error("探测不得读取音频正文")
	return 0, io.EOF
}
func (b *unreadBody) Close() error { b.closed = true; return nil }
func TestProbeSafeOutputNoPlaybackSideEffects(t *testing.T) {
	f := newFixture(t)
	setupProbe(t, f)
	_, key := f.create(t, true)
	body := &unreadBody{t: t}
	f.s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Range") != "bytes=0-0" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error(r.Header)
		}
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Type": {"audio/mpeg; secret=NEVER_EXPORT"}, "Content-Length": {"1"}, "Content-Range": {"bytes 0-0/123"}, "Accept-Ranges": {"bytes"}, "Etag": {"NEVER_EXPORT"}, "Location": {"http://SECRET_PATH"}, "Set-Cookie": {"SECRET_COOKIE"}}, Body: body, Request: r}, nil
	})}
	before, _ := f.s.DB.Statistics(context.Background())
	settingsBefore, _ := json.Marshal(f.s.Settings.Get())
	w := f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil)
	if w.Code != 200 || !body.closed {
		t.Fatal(w.Code, w.Body, body.closed)
	}
	for _, secret := range []string{"NEVER_EXPORT", "SECRET_", "秘密", "https://", "media.example", "signature", "Etag"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("泄露", secret, w.Body)
		}
	}
	after, _ := f.s.DB.Statistics(context.Background())
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("探测改变持久化统计", string(b), string(a))
	}
	settingsAfter, _ := json.Marshal(f.s.Settings.Get())
	if string(settingsBefore) != string(settingsAfter) {
		t.Fatal("修改配置")
	}
	if _, err := f.s.DB.GetTrack(context.Background(), "tr-wy-1"); err == nil {
		t.Fatal("探测不应持久化播放歌曲")
	}
	for _, body := range []string{`{"trackId":"http://127.0.0.1/secret","quality":"320k"}`, `{"trackId":"tr-wy-1?secret=x","quality":"320k"}`, `{"trackId":"tr-wy-1","quality":"320k","url":"http://127.0.0.1"}`, `{"trackId":"tr-wy-1","quality":"320k","headers":{}}`, `{"trackId":"tr-wy-1","quality":"script"}`, `{"trackId":"tr-wy-1"}`} {
		if w := f.call("POST", "/api/debug/probe", body, key, nil); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
}
func TestProbeDoesNotAutomaticallyRefreshOrFailOver(t *testing.T) {
	f := newFixture(t)
	setupProbe(t, f)
	const fallback = `lx.on(lx.EVENT_NAMES.request,()=>Promise.resolve('https://media.example/other'));lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`
	if _, err := f.s.Catalog.Sources.Load(context.Background(), 22, 2, fallback); err != nil {
		t.Fatal(err)
	}
	_, key := f.create(t, true)
	calls := 0
	body := &unreadBody{t: t}
	f.s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("User-Agent") != "lxsc-debug-probe" || r.Header.Get("Referer") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Range") != "bytes=0-0" {
			t.Error("主动探测不得改用播放请求头或继承凭据")
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: body, Request: r}, nil
	})}
	w := f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil)
	var result struct {
		Events []Event `json:"events"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || calls != 1 || !body.closed || len(result.Events) != 3 {
		t.Fatal("探测应保持独立三阶段且只检查一次音频响应")
	}
	for i, stage := range []string{"metadata", "resolve", "probe"} {
		if result.Events[i].Stage != stage {
			t.Fatalf("探测不应自动加入播放恢复阶段：%+v", result.Events)
		}
	}
	if result.Events[2].Status != 403 {
		t.Fatal("必须忠实交付本次探测的上游拒绝状态")
	}
	info, err := f.s.Catalog.Track(context.Background(), "tr-wy-1")
	if err != nil {
		t.Fatal(err)
	}
	cached, err := f.s.Catalog.ResolvePlaybackURL(context.Background(), info, "320k")
	if err != nil || !cached.Cached || cached.Result.SourceID != 1 {
		t.Fatal("独立探测不能偷偷刷新或切换音源")
	}
	if _, err := f.s.DB.GetTrack(context.Background(), "tr-wy-1"); err == nil {
		t.Fatal("探测不能记录播放")
	}
}

func TestProbeRevokeExpiryAndOwnerCancelInFlight(t *testing.T) {
	for _, action := range []string{"revoke", "expiry", "demote", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			setupProbe(t, f)
			v, key := f.create(t, true)
			started := make(chan struct{})
			cancelled := make(chan struct{})
			f.s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				close(started)
				<-r.Context().Done()
				close(cancelled)
				return nil, r.Context().Err()
			})}
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				result <- f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil)
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("探测未启动")
			}
			switch action {
			case "revoke":
				f.s.Tokens.Revoke(v.ID)
			case "expiry":
				f.s.Tokens.mu.Lock()
				token := f.s.Tokens.entries[v.ID]
				token.ExpiresAt = time.Now()
				token.timer.Reset(time.Millisecond)
				f.s.Tokens.mu.Unlock()
			case "demote":
				if err := f.s.DB.UpdateUser(context.Background(), f.admin.ID, f.admin.Name, "", false, "320k"); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err := f.s.DB.DeleteUser(context.Background(), f.admin.ID); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("未取消在途HTTP")
			}
			select {
			case w := <-result:
				if w.Code != 408 {
					t.Fatal(w.Code, w.Body)
				}
			case <-time.After(time.Second):
				t.Fatal("响应未结束")
			}
		})
	}
}

func TestSlowRequestBodyCancelledByRevocation(t *testing.T) {
	f := newFixture(t)
	view, key := f.create(t, true)
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.router.ServeHTTP(w, r); close(finished) }))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 合法请求头后故意只传一小段正文，不依靠客户端关闭来释放服务器工作。
	_, err = fmt.Fprintf(conn, "POST /api/debug/probe HTTP/1.1\r\nHost: debug.test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 2000\r\n\r\n{", key)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Second)
	for len(f.s.concurrent) != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(f.s.concurrent) != 1 {
		t.Fatal("请求未进入限流边界")
	}
	f.s.Tokens.Revoke(view.ID)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("慢正文未随撤销取消")
	}
	if len(f.s.concurrent) != 0 {
		t.Fatal("并发槽未释放")
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatal("撤销后TCP连接未结束", err)
	}
}

func TestActualConcurrentRequestAndProbeLimits(t *testing.T) {
	f := newFixture(t)
	_, key := f.create(t, false)
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	results := make(chan int, 8)
	handler := f.s.authorize(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		output(w, 200, map[string]bool{"ok": true})
	}))
	for i := 0; i < 8; i++ {
		go func() {
			r := httptest.NewRequest("GET", "http://debug.test/api/debug/status", nil)
			r.Header.Set("Authorization", "Bearer "+key)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			results <- w.Code
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("并发请求未开始")
		}
	}
	w := f.call("GET", "/api/debug/status", "", key, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Error(w.Code, w.Header())
	}
	close(release)
	for i := 0; i < 8; i++ {
		if code := <-results; code != 200 {
			t.Error(code)
		}
	}
	setupProbe(t, f)
	view, key := f.create(t, true)
	probeStarted := make(chan struct{}, 2)
	probeResults := make(chan int, 2)
	f.s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		probeStarted <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	for i := 0; i < 2; i++ {
		go func() {
			probeResults <- f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil).Code
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-probeStarted:
		case <-time.After(time.Second):
			t.Fatal("并发探测未开始")
		}
	}
	w = f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Error(w.Code, w.Header())
	}
	f.s.Tokens.Revoke(view.ID)
	for i := 0; i < 2; i++ {
		select {
		case code := <-probeResults:
			if code != 408 {
				t.Error(code)
			}
		case <-time.After(time.Second):
			t.Fatal("探测撤销未释放")
		}
	}
}
