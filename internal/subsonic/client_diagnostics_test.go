package subsonic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/diagnostics"
)

func TestClientDiagnosticsRealRoutesAndPrivacy(t *testing.T) {
	f := lyricTestServer(t, lyricTestSDK)
	f.server.Diagnostics = &diagnostics.Events{}
	if err := f.database.CreateAPIKey(context.Background(), f.user.ID, "private-client-token", "test"); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Mount("/rest", f.server.Routes())
	for _, tc := range []struct {
		endpoint, format, client string
		post                     bool
		want                     int
	}{
		{"getLyricsBySongId", "json", "Amcfy Music", false, 3},
		{"getLyrics", "xml", "StreamMusic", true, 4},
	} {
		params := url.Values{"apiKey": {"private-client-token"}, "id": {"tr-wy-lyric"}, "f": {tc.format}, "c": {tc.client}}
		target := "/rest/" + tc.endpoint + ".view"
		method := http.MethodGet
		body := ""
		if tc.post {
			method = http.MethodPost
			body = params.Encode()
		} else {
			target += "?" + params.Encode()
		}
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		events := f.server.Diagnostics.List()
		event := events[len(events)-1]
		if event.Endpoint != tc.endpoint || event.Format != tc.format || event.Result != "ok" || event.Lines == nil || *event.Lines != tc.want {
			t.Fatalf("响应未正确记录: %+v / %s", event, rec.Body)
		}
		wantClient := "amcfy"
		if tc.post {
			wantClient = "stream_music"
		}
		if event.Client != wantClient {
			t.Fatal(event)
		}
	}
	req := httptest.NewRequest("GET", "/rest/getPlaylist?apiKey=private-client-token&id=pl-PRIVATE&c=PRIVATE&f=json", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)
	events := f.server.Diagnostics.List()
	last := events[len(events)-1]
	if last.Result != "failed" || last.ProtocolCode == nil || *last.ProtocolCode != ErrNotFound {
		t.Fatal(last)
	}
	// 认证失败也要记录，但不能记录传入的用户名/口令/原始客户端名。
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/rest/getPlaylist?apiKey=PRIVATE&c=Amcfy&f=json&id=lb-wy-one", nil))
	events = f.server.Diagnostics.List()
	last = events[len(events)-1]
	if last.ProtocolCode == nil || *last.ProtocolCode != 44 || last.BoardID != "lb-wy-one" {
		t.Fatal(last)
	}
	raw, _ := json.Marshal(events)
	for _, secret := range []string{"PRIVATE", "private-client-token", "测试歌曲", "测试歌手", "第一行", "First line", "apiKey", "pl-"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("事件泄露 %s", secret)
		}
	}
}

func TestClientDiagnosticsBoardCountsAndEmpty(t *testing.T) {
	f := newDirectoryTestServer(t)
	f.server.Diagnostics = &diagnostics.Events{}
	if err := f.database.CreateAPIKey(context.Background(), f.user.ID, "client-key", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"boardSources": json.RawMessage(`["wy"]`)}); err != nil {
		t.Fatal(err)
	}
	f.server.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		if strings.HasSuffix(path, ".getBoards") {
			return json.RawMessage(`{"list":[{"name":"PRIVATE榜单","bangid":"empty"}]}`), nil
		}
		return json.RawMessage(`{"list":[]}`), nil
	})
	router := chi.NewRouter()
	router.Mount("/rest", f.server.Routes())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/rest/getPlaylist?apiKey=client-key&id=lb-wy-empty&c=Amcfy&f=json", nil))
	event := f.server.Diagnostics.List()[0]
	if event.Stage != "client_response" || event.Count != nil || event.Result != "failed" || event.Status != 503 || event.BoardID != "lb-wy-empty" {
		t.Fatalf("空榜单事件错误: %+v", event)
	}
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "2" || rec.Header().Get("Cache-Control") != "no-store" || event.ProtocolCode == nil || *event.ProtocolCode != ErrGeneric {
		t.Fatalf("空榜单必须返回可重试且不可缓存的协议失败: %+v %v", event, rec.Header())
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/rest/getPlaylists?apiKey=client-key&c=Amcfy&f=json", nil))
	event = f.server.Diagnostics.List()[1]
	if len(event.BoardIDs) != 1 || event.BoardIDs[0] != "lb-wy-empty" || event.Count == nil || *event.Count != 1 {
		t.Fatalf("缺少可供定向探测的公开榜单ID: %+v", event)
	}
}
