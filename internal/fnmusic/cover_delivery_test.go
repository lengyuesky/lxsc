package fnmusic

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"

	"lxsc/internal/music"
)

func coverRequest(f *fixture, token string, query url.Values, validator string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/music/api/v1/static/cover?"+query.Encode(), nil)
	r.AddCookie(&http.Cookie{Name: "music-token", Value: token})
	if validator != "" {
		r.Header.Set("If-None-Match", validator)
	}
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	return w
}

func TestCoverEdgeFollowsOfficialVariants(t *testing.T) {
	for size, want := range map[string]int{
		"": 640, "abc": 640, "-1": 640, "0": 640, "1": 640, "63": 640,
		"64": 120, "120": 120, "121": 160, "160": 160, "161": 640, "320": 640, "640": 640, "1600": 640,
	} {
		if got := coverEdge(size); got != want {
			t.Errorf("coverEdge(%q) = %d, want %d", size, got, want)
		}
	}
}

func TestCoverRequestsSizedVariantAndCachesBoundIdentifiers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	track := music.FromMap(map[string]any{
		"source": "wy", "songmid": "sized", "name": "synthetic-title", "singer": "synthetic-artist",
		"albumName": "synthetic-album", "img": "http://p2.music.126.net/key==/1.jpg",
	})
	bare := music.FromMap(map[string]any{"source": "wy", "songmid": "bare", "name": "synthetic-bare", "singer": "synthetic-artist"})
	if err := f.server.Catalog.RememberSync(ctx, []*music.Info{track, bare}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreatePlaylist(ctx, "pl-sized", f.users["alice"].ID, "sized", []string{track.TrackID(), bare.TrackID()}); err != nil {
		t.Fatal(err)
	}
	token := login(t, f, "alice")
	identifyCovers(t, f, token)
	picture := coverPNG(t)
	var mu sync.Mutex
	var requested []string
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		requested = append(requested, r.URL.String())
		mu.Unlock()
		return imageResponse(http.StatusOK, picture), nil
	})}
	guid := identifier("track", track.TrackID())
	current := f.server.coverID("track", guid, track)
	const address = "https://p2.music.126.net/key==/1.jpg"
	variant := func(edge string) string {
		return address + "?imageView&thumbnail=" + edge + "x" + edge + "&type=jpg&quality=85"
	}

	// 列表下发的当前标识：按请求尺寸取缩略图，客户端可长期缓存。
	// 归入同一档的尺寸（320、缺省、1600）共用一次上游请求。
	for _, tc := range []struct {
		size string
		want []string
	}{{"120", []string{variant("120")}}, {"160", []string{variant("160")}}, {"320", []string{variant("640")}}, {"", nil}, {"1600", nil}} {
		requested = nil
		query := url.Values{"coverId": {current}}
		if tc.size != "" {
			query.Set("size", tc.size)
		}
		w := coverRequest(f, token, query, "")
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) || !slices.Equal(requested, tc.want) {
			t.Fatalf("size=%q 没有请求预期的缩略图: status=%d requests=%v", tc.size, w.Code, requested)
		}
		if w.Header().Get("Cache-Control") != "private, max-age=604800" || w.Header().Get("ETag") == "" {
			t.Fatalf("当前标识的原图未允许长期缓存: %v", w.Header())
		}
	}

	// 同一尺寸再次请求不访问上游；带校验值时只返回 304。
	requested = nil
	first := coverRequest(f, token, url.Values{"coverId": {current}, "size": {"640"}}, "")
	again := coverRequest(f, token, url.Values{"coverId": {current}, "size": {"640"}}, first.Header().Get("ETag"))
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 || len(requested) != 0 {
		t.Fatalf("未变的封面被重复下载: status=%d requests=%v", again.Code, requested)
	}
	events := f.server.Diagnostics.List()
	if last := events[len(events)-1]; last.Status != http.StatusNotModified || last.Result != "ok" || !last.Cached || last.BytesWritten == nil || *last.BytesWritten != 0 {
		t.Fatalf("304 的诊断记录不正确: %+v", last)
	}

	// 旧格式标识和已过期的版本仍可读取，但不能长期缓存。
	changed := music.FromMap(map[string]any{"source": "wy", "songmid": "sized", "name": "renamed", "img": "http://p2.music.126.net/key==/1.jpg"})
	for _, id := range []string{"track_" + guid, f.server.coverID("track", guid, changed)} {
		w := coverRequest(f, token, url.Values{"coverId": {id}, "size": {"640"}}, "")
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, max-age=900" {
			t.Fatalf("未绑定当前元数据的标识被长期缓存: %s %v", id, w.Header())
		}
	}
	if f.server.boundCover(f.server.coverID("track", identifier("track", bare.TrackID()), bare), bare) {
		t.Fatal("元数据没有图片地址时，标识不能视为已绑定图片")
	}
}

func TestUploadedCoverRevalidatesWithoutResendingImage(t *testing.T) {
	f := newFixture(t)
	alice, bob := login(t, f, "alice"), login(t, f, "bob")
	picture := coverPNG(t)
	cover := payload(t, uploadCover(t, f, alice, picture))["coverId"].(string)
	query := url.Values{"coverId": {cover}}
	first := coverRequest(f, alice, query, "")
	etag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), picture) || etag != `"`+cover+`"` || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("上传封面缺少校验值或改变了缓存策略: %d %v", first.Code, first.Header())
	}
	if w := coverRequest(f, alice, query, etag); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatal("未变的上传封面被重复传输", w.Code)
	}
	// 校验值不是授权：其他用户即使猜到也必须先通过权限检查。
	if w := coverRequest(f, bob, query, etag); w.Code != http.StatusNotFound {
		t.Fatal("校验值绕过了上传封面的权限检查", w.Code)
	}
}
