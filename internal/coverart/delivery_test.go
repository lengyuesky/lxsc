package coverart

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
)

func syntheticPNG(t *testing.T, edge int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, edge, edge))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func pictureResponse(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
}

// serveResource 走与协议层相同的诊断包装，返回响应和写出记录。
func serveResource(s *Service, resource Resource, header http.Header) (*httptest.ResponseRecorder, *diagnostics.Event) {
	r := httptest.NewRequest(http.MethodGet, "/cover", nil)
	for name, values := range header {
		r.Header[name] = values
	}
	event := &diagnostics.Event{Format: "binary"}
	w := httptest.NewRecorder()
	capture, finish := diagnostics.CaptureResponse(w, r, event)
	s.Serve(capture, r, resource, event)
	finish()
	return w, event
}

func TestSizedAddressOnlyRewritesKnownImageHosts(t *testing.T) {
	const suffix = "?imageView&thumbnail=160x160&type=jpg&quality=85"
	for address, want := range map[string]string{
		"https://p2.music.126.net/key==/1.jpg":           "https://p2.music.126.net/key==/1.jpg" + suffix,
		"http://P1.MUSIC.126.NET/key==/1.jpg":            "http://P1.MUSIC.126.NET/key==/1.jpg" + suffix,
		"https://p2.music.126.net/key==/1.jpg?param=90y": "https://p2.music.126.net/key==/1.jpg?param=90y",
		"https://p2.music.126.net/key==/1.jpg?":          "https://p2.music.126.net/key==/1.jpg?",
		"https://music.126.net.example/key/1.jpg":        "https://music.126.net.example/key/1.jpg",
		"https://y.gtimg.cn/music/photo_new/T002.jpg":    "https://y.gtimg.cn/music/photo_new/T002.jpg",
		"file:///p2.music.126.net/1.jpg":                 "file:///p2.music.126.net/1.jpg",
		"://bad":                                         "://bad",
		"":                                               "",
	} {
		if got := sizedAddress(address, 160); got != want {
			t.Errorf("sizedAddress(%q) = %q, want %q", address, got, want)
		}
	}
	if got := sizedAddress("https://p2.music.126.net/key==/1.jpg", 0); got != "https://p2.music.126.net/key==/1.jpg" {
		t.Fatal("未指定尺寸时不应改写原图地址", got)
	}
}

func TestCoverThumbnailFallsBackToOriginalWithoutHidingUnsafeTargets(t *testing.T) {
	original, thumbnail := syntheticPNG(t, 8), syntheticPNG(t, 2)
	var mu sync.Mutex
	var requested []string
	s := &Service{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requested = append(requested, r.URL.String())
		mu.Unlock()
		switch {
		case r.URL.Path == "/unsafe.jpg":
			return nil, httpguard.ErrUnsafeTarget
		case r.URL.Path == "/flaky.jpg" && r.URL.RawQuery != "":
			return pictureResponse(http.StatusServiceUnavailable, []byte("thumbnail unavailable")), nil
		case r.URL.RawQuery != "":
			return pictureResponse(http.StatusOK, thumbnail), nil
		default:
			return pictureResponse(http.StatusOK, original), nil
		}
	})}}
	const variant = "?imageView&thumbnail=160x160&type=jpg&quality=85"
	for _, tc := range []struct {
		address string
		edge    int
		status  int
		body    []byte
		want    []string
	}{
		{"https://p2.music.126.net/ok.jpg", 160, 200, thumbnail, []string{"https://p2.music.126.net/ok.jpg" + variant}},
		{"https://p2.music.126.net/flaky.jpg", 160, 200, original, []string{"https://p2.music.126.net/flaky.jpg" + variant, "https://p2.music.126.net/flaky.jpg"}},
		{"https://p2.music.126.net/unsafe.jpg", 160, 502, nil, []string{"https://p2.music.126.net/unsafe.jpg" + variant}},
		{"https://image.example/other.jpg", 160, 200, original, []string{"https://image.example/other.jpg"}},
		{"https://p2.music.126.net/full.jpg", 0, 200, original, []string{"https://p2.music.126.net/full.jpg"}},
	} {
		requested = nil
		w, event := serveResource(s, Resource{UserID: 1, Address: tc.address, MaxEdge: tc.edge}, nil)
		if w.Code != tc.status || !slices.Equal(requested, tc.want) {
			t.Fatalf("%s: status=%d requests=%v, want %d %v", tc.address, w.Code, requested, tc.status, tc.want)
		}
		if tc.body != nil && (!bytes.Equal(w.Body.Bytes(), tc.body) || event.CoverOrigin != "original" || event.Result != "ok") {
			t.Fatalf("%s: 返回了错误的图片或来源: %+v", tc.address, event)
		}
	}
	// 同一张图的不同尺寸分别缓存，重复请求不再访问上游。
	requested = nil
	for _, edge := range []int{160, 160, 640, 640} {
		if w, _ := serveResource(s, Resource{UserID: 1, Address: "https://p2.music.126.net/ok.jpg", MaxEdge: edge}, nil); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if want := []string{"https://p2.music.126.net/ok.jpg?imageView&thumbnail=640x640&type=jpg&quality=85"}; !slices.Equal(requested, want) {
		t.Fatalf("尺寸缓存未按预期复用: %v", requested)
	}
}

func TestCoverOriginalIsSharedAcrossFallbackFields(t *testing.T) {
	picture := syntheticPNG(t, 4)
	var mu sync.Mutex
	calls := map[string]int{}
	s := &Service{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls[r.URL.Host]++
		mu.Unlock()
		if r.URL.Host == "broken.example" {
			return pictureResponse(http.StatusForbidden, nil), nil
		}
		return pictureResponse(http.StatusOK, picture), nil
	})}}
	const template = "https://fallback.example/cover"
	track := map[string]string{"title": "synthetic-title", "artist": "synthetic-artist", "album": "synthetic-album"}
	album := map[string]string{"artist": "synthetic-artist", "album": "synthetic-album"}
	artist := map[string]string{"artist": "synthetic-artist"}
	for i, fields := range []map[string]string{track, album, artist, nil} {
		w, event := serveResource(s, Resource{UserID: 1, Address: "https://image.example/cover", Template: template, Fields: fields}, nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), picture) || event.CoverOrigin != "original" || event.Cached != (i > 0) {
			t.Fatalf("第 %d 个资源没有共用已取回的原图: %+v", i, event)
		}
	}
	if calls["image.example"] != 1 || calls["fallback.example"] != 0 {
		t.Fatalf("同一原图被重复下载: %v", calls)
	}
	// 原图失败时，备用接口的结果仍按各自的匹配字段分开，不能互相冒用。
	for _, fields := range []map[string]string{track, album, track} {
		w, event := serveResource(s, Resource{UserID: 1, Address: "https://broken.example/cover", Template: template, Fields: fields}, nil)
		if w.Code != 200 || event.CoverOrigin != "custom" {
			t.Fatalf("备用封面未生效: %+v", event)
		}
	}
	if calls["fallback.example"] != 2 {
		t.Fatalf("备用封面应按匹配字段各取一次: %v", calls)
	}
}

func TestCoverConditionalRequestsAndClientCacheLifetime(t *testing.T) {
	picture := syntheticPNG(t, 4)
	var fail bool
	s := &Service{HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if fail || r.URL.Host == "broken.example" {
			return nil, errors.New("temporary failure")
		}
		return pictureResponse(http.StatusOK, picture), nil
	})}}
	resource := Resource{UserID: 1, Address: "https://image.example/cover", Fields: map[string]string{"title": "synthetic"}}
	first, _ := serveResource(s, resource, nil)
	etag := first.Header().Get("ETag")
	if first.Code != 200 || len(etag) != 34 || first.Header().Get("Cache-Control") != "private, max-age=900" {
		t.Fatal("默认封面响应缺少校验值或改变了缓存时长", first.Code, etag, first.Header().Get("Cache-Control"))
	}
	for _, validator := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		w, event := serveResource(s, resource, http.Header{"If-None-Match": {validator}})
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "private, max-age=900" || w.Header().Get("Content-Length") != "" {
			t.Fatalf("未变封面没有返回 304: validator=%s status=%d header=%v", validator, w.Code, w.Header())
		}
		if event.Result != "ok" || event.Status != http.StatusNotModified || event.BytesWritten == nil || *event.BytesWritten != 0 || event.ResponseBytes == nil || *event.ResponseBytes != 0 || event.CoverOrigin != "original" {
			t.Fatalf("304 的写出记录不正确: %+v", event)
		}
	}
	if w, _ := serveResource(s, resource, http.Header{"If-None-Match": {`"0123456789abcdef0123456789abcdef"`}}); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), picture) {
		t.Fatal("校验值不同仍应返回完整图片", w.Code)
	}

	// 只有协议层确认标识绑定图片地址的原图可以延长客户端缓存。
	resource.MaxAge = 7 * 24 * time.Hour
	if w, _ := serveResource(s, resource, nil); w.Header().Get("Cache-Control") != "private, max-age=604800" {
		t.Fatal("已绑定的原图未延长客户端缓存", w.Header().Get("Cache-Control"))
	}
	custom := Resource{UserID: 1, Address: "https://broken.example/cover", Template: "https://fallback.example/cover", Fields: resource.Fields, MaxAge: resource.MaxAge}
	if w, event := serveResource(s, custom, nil); event.CoverOrigin != "custom" || w.Header().Get("Cache-Control") != "private, max-age=900" || w.Header().Get("ETag") == "" {
		t.Fatalf("备用封面不应长期缓存: %+v %v", event, w.Header())
	}

	// 上游故障时的旧图每次重新确认，默认图不可缓存也不提供校验值。
	c := s.cache
	c.mu.Lock()
	for _, key := range c.entries.Keys() {
		entry, _ := c.entries.Peek(key)
		entry.expires = time.Now().Add(-time.Minute)
		c.entries.Add(key, entry)
	}
	c.mu.Unlock()
	fail = true
	stale, event := serveResource(s, resource, nil)
	if stale.Code != 200 || event.CoverOrigin != "stale" || stale.Header().Get("Cache-Control") != "private, no-cache, max-age=0" || stale.Header().Get("ETag") != etag {
		t.Fatalf("旧图响应不正确: %+v %v", event, stale.Header())
	}
	if w, event := serveResource(s, resource, http.Header{"If-None-Match": {etag}}); w.Code != http.StatusNotModified || event.Result != "unavailable" {
		t.Fatalf("旧图未变时应返回 304 并保留降级记录: %d %+v", w.Code, event)
	}
	missing := Resource{UserID: 1, Address: "https://broken.example/missing", Fields: resource.Fields, MaxAge: resource.MaxAge}
	placeholder, event := serveResource(s, missing, http.Header{"If-None-Match": {"*"}})
	if placeholder.Code != 200 || event.CoverOrigin != "placeholder" || placeholder.Header().Get("ETag") != "" || !strings.Contains(placeholder.Header().Get("Cache-Control"), "no-store") || placeholder.Body.Len() == 0 {
		t.Fatalf("默认图不能被缓存或以 304 确认: %+v %v", event, placeholder.Header())
	}
}
