package subsonic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/music"
)

func TestAdvertisedBoardCoverWorksWithoutLoadingTracks(t *testing.T) {
	f := newDirectoryTestServer(t)
	ctx := context.Background()
	if _, err := f.store.Update(ctx, map[string]json.RawMessage{"boardSources": json.RawMessage(`["wy"]`)}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	remote := func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		calls.Add(1)
		switch path {
		case "wy.leaderboard.getBoards":
			return json.RawMessage(`{"list":[{"name":"测试榜单","bangid":"one"}]}`), nil
		case "wy.leaderboard.getList":
			return json.RawMessage(`{"list":[{"songmid":"one","name":"测试歌曲","interval":"00:30"}],"total":1,"limit":100,"page":1}`), nil
		default:
			return nil, fmt.Errorf("封面不应调用上游: %s", path)
		}
	}
	f.server.Catalog.SetRemoteCallerForTest(remote)
	root := f.boardRequest(t, f.server.getPlaylists, "/rest/getPlaylists?f=json")
	summary := root["playlists"].(map[string]any)["playlist"].([]any)[0].(map[string]any)
	coverID, ok := summary["coverArt"].(string)
	if !ok || coverID == "" || calls.Load() != 1 {
		t.Fatalf("列表应只读取榜单名称并公布可用的封面 ID: %+v calls=%d", summary, calls.Load())
	}
	var previous []byte
	check := func() {
		t.Helper()
		before := calls.Load()
		for _, mode := range []string{"redirect", "proxy"} {
			if _, err := f.store.Update(ctx, map[string]json.RawMessage{"coverMode": json.RawMessage(strconv.Quote(mode))}); err != nil {
				t.Fatal(err)
			}
			for _, size := range []string{"100", "600"} {
				req := httptest.NewRequest(http.MethodGet, "/rest/getCoverArt?id="+coverID+"&size="+size, nil)
				req = req.WithContext(withUser(req.Context(), f.user))
				rec := httptest.NewRecorder()
				f.server.getCoverArt(rec, req)
				picture, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
				if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || err != nil || picture.Bounds().Empty() {
					t.Fatalf("公布的榜单封面必须是可解码的图片: status=%d headers=%v err=%v", rec.Code, rec.Header(), err)
				}
				if rec.Header().Get("Location") != "" || rec.Header().Get("Content-Length") != strconv.Itoa(rec.Body.Len()) || rec.Header().Get("Cache-Control") != "private, max-age=86400" {
					t.Fatalf("内置封面不应重定向，且应提供明确长度与客户端缓存: %v", rec.Header())
				}
				if previous != nil && !bytes.Equal(previous, rec.Body.Bytes()) {
					t.Fatal("同一内置封面不应随加载、缓存或代理方式变化")
				}
				previous = bytes.Clone(rec.Body.Bytes())
			}
		}
		if calls.Load() != before {
			t.Fatalf("封面请求额外访问了上游: before=%d after=%d", before, calls.Load())
		}
	}
	check()
	detailRoot := f.boardRequest(t, f.server.getPlaylist, "/rest/getPlaylist?f=json&id=lb-wy-one")
	detail, ok := detailRoot["playlist"].(map[string]any)
	if !ok {
		t.Fatalf("榜单详情请求失败: %+v", detailRoot)
	}
	if detail["coverArt"] != coverID || detail["songCount"] != float64(1) {
		t.Fatalf("详情与目录的封面契约不一致: %+v", detail)
	}
	check()
	f.server.Catalog.StopBoardWarm()
	catalog := music.NewCatalog(f.database, nil, nil, f.store, f.server.Log)
	catalog.SetRemoteCallerForTest(remote)
	t.Cleanup(catalog.StopBoardWarm)
	f.server.Catalog = catalog
	check()
}

func TestBoardCoverRoutesKeepAuthenticationAndValidateIDs(t *testing.T) {
	f := newDirectoryTestServer(t)
	if err := f.database.CreateAPIKey(context.Background(), f.user.ID, "cover-test-key", "test"); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Mount("/rest", f.server.Routes())
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, key := range []string{"", "wrong-key", "cover-test-key"} {
			params := url.Values{"apiKey": {key}, "id": {"lb-wy-one"}, "f": {"json"}}
			target, body := "/rest/getCoverArt.view", ""
			if method == http.MethodGet {
				target += "?" + params.Encode()
			} else {
				body = params.Encode()
			}
			req := httptest.NewRequest(method, target, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if image := rec.Header().Get("Content-Type") == "image/png"; image != (key == "cover-test-key") {
				t.Fatalf("封面请求的鉴权行为错误: method=%s authenticated=%v status=%d", method, key == "cover-test-key", rec.Code)
			}
		}
	}
	for _, id := range []string{"lb-unknown-one", "lb-wy", "lb-wy-", "lb-wy-0"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/rest/getCoverArt?apiKey=cover-test-key&id="+id, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("无效榜单 ID 不应返回封面: id=%s status=%d", id, rec.Code)
		}
	}
}
