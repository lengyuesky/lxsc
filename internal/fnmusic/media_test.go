package fnmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/diagnostics"
	"lxsc/internal/js"
	"lxsc/internal/mediastream"
)

func attachMedia(t *testing.T, f *fixture) (*atomic.Int64, []byte) {
	t.Helper()
	body := []byte("synthetic-audio-for-range-test")
	var sent atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		count := &byteCounter{ResponseWriter: w, total: &sent}
		http.ServeContent(count, r, "tone.wav", time.Unix(0, 0), bytes.NewReader(body))
	}))
	t.Cleanup(upstream.Close)
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	client := upstream.Client()
	sources := js.NewSourceManager(string(prelude), client, client, f.server.Catalog.Log)
	t.Cleanup(sources.UnloadAll)
	script := fmt.Sprintf(`lx.on(lx.EVENT_NAMES.request, () => Promise.resolve(%q));
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{name:'test',type:'music',actions:['musicUrl'],qualitys:['320k']}}});`, upstream.URL+"/tone.wav")
	if _, err := sources.Load(context.Background(), 1, script); err != nil {
		t.Fatal(err)
	}
	f.server.Catalog.Sources = sources
	f.server.Media = &mediastream.Service{Catalog: f.server.Catalog, Settings: f.server.Settings, Log: f.server.Catalog.Log, HTTP: client, Diagnostics: &diagnostics.Events{}}
	return &sent, body
}

type byteCounter struct {
	http.ResponseWriter
	total *atomic.Int64
}

func (w *byteCounter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.total.Add(int64(n))
	return n, err
}

func TestMediaAuthenticationAndServerMode(t *testing.T) {
	f := newFixture(t)
	sent, body := attachMedia(t, f)
	token := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	guid := identifier("track", "tr-wy-one")
	if request(f, http.MethodGet, "/music/api/v1/track/stream?guid="+guid+"&token="+token, "", nil).Code != http.StatusUnauthorized {
		t.Fatal("未支持的 query 票据绕过认证")
	}
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		value, _ := json.Marshal(mode)
		if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": value}); err != nil {
			t.Fatal(err)
		}
		w := request(f, http.MethodGet, "/music/api/v1/track/stream?guid="+guid+"&proxy=1&id=tr-wy-two&u=admin", token, nil)
		if mode == "proxy" {
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) {
				t.Fatal("管理员代理模式未使用共享交付")
			}
		} else {
			if w.Code != http.StatusFound || w.Header().Get("Location") == "" {
				t.Fatalf("%s 没有重定向", mode)
			}
			if strings.Contains(w.Header().Get("Location"), token) {
				t.Fatal("把内部会话泄漏到音频上游")
			}
			if w.Header().Get("Referrer-Policy") != "no-referrer" {
				t.Fatal("重定向缺少来源保护")
			}
		}
	}
	if sent.Load() < int64(len(body)) {
		t.Fatal("代理没有实际交付音频")
	}
	events := f.server.Media.Diagnostics.List()
	var associated string
	for _, event := range events {
		if event.Stage == "client_response" && event.Client == "fnmusic" && event.Status == http.StatusOK {
			associated = event.RequestID
		}
	}
	if associated == "" {
		t.Fatal("飞牛媒体响应没有关联诊断标识")
	}
	matched := false
	for _, event := range events {
		if event.Stage == "metadata" && event.RequestID == associated {
			matched = true
		}
	}
	if !matched {
		t.Fatal("媒体取链和响应诊断失去关联")
	}
	raw, _ := json.Marshal(events)
	for _, sensitive := range []string{token, "alice", "synthetic-audio", "127.0.0.1"} {
		if strings.Contains(string(raw), sensitive) {
			t.Fatal("媒体诊断泄漏凭据或访问地址")
		}
	}
}

func TestMediaHeadAndNonzeroRange(t *testing.T) {
	f := newFixture(t)
	_, body := attachMedia(t, f)
	token := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	guid := identifier("track", "tr-wy-one")
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)}); err != nil {
		t.Fatal(err)
	}
	w := request(f, http.MethodHead, "/music/api/v1/track/stream/"+guid, token, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != fmt.Sprint(len(body)) || w.Body.Len() != 0 {
		t.Fatal("HEAD 元数据或正文异常")
	}
	r := httptest.NewRequest(http.MethodGet, "/music/api/v1/track/stream?guid="+guid, nil)
	r.Header.Set("Authorization", token)
	r.Header.Set("Range", "bytes=5-9")
	w = httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), body[5:10]) || !strings.HasPrefix(w.Header().Get("Content-Range"), "bytes 5-9/") {
		t.Fatal("非零字节范围没有透传")
	}
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"force_redirect"`)}); err != nil {
		t.Fatal(err)
	}
	w = request(f, http.MethodHead, "/music/api/v1/track/stream?guid="+guid, token, nil)
	if w.Code != http.StatusFound || w.Body.Len() != 0 {
		t.Fatal("强制 302 HEAD 合约被覆盖")
	}
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)}); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest(http.MethodGet, "/music/api/v1/track/stream?guid="+guid, nil)
	r.Header.Set("Authorization", token)
	r.Header.Set("Range", "bytes=999999-")
	w = httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatal("无效范围没有保留 416")
	}
}

func TestLoginRejectsCrossSiteAndMalformedBody(t *testing.T) {
	f := newFixture(t)
	for _, test := range []struct {
		contentType, origin, body string
		status                    int
	}{
		{"application/x-www-form-urlencoded", "", "username=alice", http.StatusUnsupportedMediaType},
		{"application/json", "https://other.example", `{}`, http.StatusForbidden},
		{"application/json", "", `{} {}`, http.StatusBadRequest},
		{"application/json", "", strings.Repeat("x", 65<<10), http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "/music/api/v1/user/password-login", strings.NewReader(test.body))
		r.Header.Set("Content-Type", test.contentType)
		if test.origin != "" {
			r.Header.Set("Origin", test.origin)
		}
		w := httptest.NewRecorder()
		f.routes.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatalf("错误正文检查状态 %d，预期 %d", w.Code, test.status)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/music/api/v1/user/me", nil)
	r.Header.Set("authx", "nonce=123456&timestamp=1&sign=not-a-session")
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("把 authx 当成用户认证")
	}
}
