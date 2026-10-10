package subsonic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/admission"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

func burstCoverFixture(t *testing.T, count int) (directoryTestServer, http.Handler, []*music.Info) {
	t.Helper()
	f := newDirectoryTestServer(t)
	enableCustomCover(t, f, settings.DefaultCustomCoverURL)
	f.server.Diagnostics = &diagnostics.Events{}
	if err := f.database.CreateAPIKey(context.Background(), f.user.ID, "cover-burst-key", "test"); err != nil {
		t.Fatal(err)
	}
	tracks := make([]*music.Info, count)
	for i := range tracks {
		tracks[i] = music.FromMap(map[string]any{"source": "wy", "songmid": fmt.Sprint("burst-", i), "name": "合成封面歌曲", "singer": "合成歌手", "img": fmt.Sprintf("https://image.example/%d", i)})
	}
	f.server.Catalog.Cache(tracks)
	router := chi.NewRouter()
	router.Mount("/rest", f.server.Routes())
	return f, router, tracks
}

func coverClientRequest(router http.Handler, id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	params := url.Values{"id": {id}, "apiKey": {"cover-burst-key"}, "c": {"Amcfy Music"}, "f": {"json"}}
	router.ServeHTTP(w, httptest.NewRequest("GET", "/rest/getCoverArt?"+params.Encode(), nil))
	return w
}

func waitCoverCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("封面并发状态未按预期推进")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCoverBurstQueuesSixDistinctImages(t *testing.T) {
	f, router, tracks := burstCoverFixture(t, 6)
	picture := customCoverPNG(t)
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	results := make(chan *httptest.ResponseRecorder, 6)
	for _, in := range tracks {
		go func() { results <- coverClientRequest(router, in.TrackID()) }()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("前四张封面未进入上游")
		}
	}
	waitCoverCondition(t, func() bool { return f.server.coverLimits.Stats().Queued == 2 })
	if stats := f.server.coverLimits.Stats(); stats.Active != 4 || stats.Rejected != 0 {
		t.Fatalf("同一用户的其他封面应排队，不能立即拒绝：%+v", stats)
	}
	unblock()
	for range 6 {
		select {
		case w := <-results:
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), picture) || w.Header().Get("X-Request-ID") == "" {
				t.Fatalf("批量请求没有返回真实封面：status=%d", w.Code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("排队中的封面未完成")
		}
	}
	if calls.Load() != 6 || f.server.coverLimits.Stats().Active != 0 || f.server.coverLimits.Stats().Queued != 0 {
		t.Fatal("请求遗漏或名额未释放")
	}
	for _, event := range f.server.Diagnostics.List() {
		if event.CoverOrigin != "original" || event.Result != "ok" || event.BytesWritten == nil || event.ResponseBytes == nil || *event.BytesWritten != int64(len(picture)) || *event.BytesWritten != *event.ResponseBytes {
			t.Fatalf("真实封面响应记录错误：%+v", event)
		}
	}
}

func TestCoverLargeImageIsCached(t *testing.T) {
	f, router, tracks := burstCoverFixture(t, 1)
	picture := append(customCoverPNG(t), make([]byte, 2<<20)...)
	var calls atomic.Int32
	f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	for range 2 {
		w := coverClientRequest(router, tracks[0].TrackID())
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), picture) {
			t.Fatal("大图未完整返回")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("超过 256 KiB 的图片仍在反复拉取", calls.Load())
	}
	events := f.server.Diagnostics.List()
	if !events[len(events)-1].Cached {
		t.Fatal("缓存命中未被记录")
	}
}

func TestCoverBusyAndQueueTimeoutReturnTemporaryImage(t *testing.T) {
	for _, queue := range []int{0, 1} {
		t.Run(fmt.Sprint(queue), func(t *testing.T) {
			f, router, tracks := burstCoverFixture(t, 1)
			f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("没有上游名额时不应绕过限制调用备用接口")
				return nil, errors.New("不应调用")
			})}
			f.server.initCoverClient()
			f.server.coverLimits = admission.New(1, queue, 1, queue)
			permit, err := f.server.coverLimits.Reserve(f.user.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer permit.Release()
			w := coverClientRequest(router, tracks[0].TrackID())
			if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Fatal("繁忙时应返回不可缓存的有效默认图", w.Code, w.Header())
			}
			if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
				t.Fatal("默认封面无法解码", err)
			}
			events := f.server.Diagnostics.List()
			last := events[len(events)-1]
			wantError := "busy"
			if queue != 0 {
				wantError = "timeout"
			}
			if last.CoverOrigin != "placeholder" || last.Result != "unavailable" || last.Error != wantError {
				t.Fatalf("默认图被误记为真实封面：%+v", last)
			}
		})
	}
}

func TestCoverPlaceholderDoesNotPoisonRecoveryOrHidePermissions(t *testing.T) {
	f, router, tracks := burstCoverFixture(t, 1)
	picture := customCoverPNG(t)
	var restored atomic.Bool
	f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if !restored.Load() {
			return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("失败"))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	w := coverClientRequest(router, tracks[0].TrackID())
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("两个图源均失败时未返回默认图")
	}
	restored.Store(true)
	w = coverClientRequest(router, tracks[0].TrackID())
	if !bytes.Equal(w.Body.Bytes(), picture) || strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("默认图污染了真实图片缓存")
	}
	other, err := f.database.CreateUser(context.Background(), "其他用户", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.database.CreatePlaylistFull(context.Background(), "pl-private-cover", other.ID, "私有歌单", "", false, []string{tracks[0].TrackID()}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pl-private-cover", "invalid-cover-id"} {
		if w := coverClientRequest(router, id); w.Code != 404 {
			t.Fatalf("未授权或未知资源不应返回默认封面：%s %d", id, w.Code)
		}
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/rest/getCoverArt?apiKey=wrong&id="+tracks[0].TrackID()+"&f=json", nil))
	if !strings.Contains(w.Header().Get("Content-Type"), "json") || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatal("鉴权失败被图片兜底掩盖")
	}
	raw, _ := json.Marshal(f.server.Diagnostics.List())
	for _, secret := range []string{"cover-burst-key", "image.example", "合成封面歌曲", "其他用户", "pl-private-cover"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("封面事件泄露了私密字段", secret)
		}
	}
}
