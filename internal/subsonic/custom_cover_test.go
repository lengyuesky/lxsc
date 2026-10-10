package subsonic

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lxsc/internal/music"
	"lxsc/internal/settings"
)

func customCoverPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func enableCustomCover(t *testing.T, f directoryTestServer, address string) {
	t.Helper()
	raw, _ := json.Marshal(address)
	if _, err := f.store.Update(context.Background(), map[string]json.RawMessage{"customCoverURL": raw}); err != nil {
		t.Fatal(err)
	}
}

func TestCustomCoverFallbackForFailedOrMissingOriginal(t *testing.T) {
	for _, failure := range []string{"403", "invalid_image", "missing", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			f := newDirectoryTestServer(t)
			enableCustomCover(t, f, settings.DefaultCustomCoverURL)
			picture := customCoverPNG(t)
			img := "https://original.example/image"
			if failure == "missing" {
				img = ""
			}
			in := music.FromMap(map[string]any{"source": "wy", "songmid": "cover", "name": "歌名&extra=1", "singer": "歌手", "albumName": "专辑", "img": img})
			f.server.Catalog.Cache([]*music.Info{in})
			calls := []string{}
			f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.URL.Host)
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
					t.Fatal("备用封面请求泄露了客户端凭据")
				}
				if r.URL.Host == "original.example" {
					if failure == "timeout" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					status := 403
					if failure == "invalid_image" {
						status = 200
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"image/png"}}, Body: io.NopCloser(strings.NewReader("<html>禁止访问</html>"))}, nil
				}
				if r.URL.Host != "api.lrc.cx" || r.URL.Path != "/cover" || r.URL.Query().Get("title") != in.Name() || r.URL.Query().Get("artist") != "歌手" || r.URL.Query().Get("album") != "专辑" || len(r.URL.Query()) != 3 {
					t.Fatal("备用封面没有按照 LrcAPI 的协议发送元数据")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
			})}
			for range 2 {
				r := httptest.NewRequest("GET", "/rest/getCoverArt?id="+in.TrackID(), nil)
				r.Header.Set("Authorization", "Bearer synthetic-secret")
				r.Header.Set("Cookie", "synthetic-cookie=secret")
				r = r.WithContext(withUser(r.Context(), f.user))
				w := httptest.NewRecorder()
				f.server.getCoverArt(w, r)
				if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), picture) || w.Header().Get("Location") != "" {
					t.Fatal("默认重定向模式下仍必须交付验证后的备用封面", w.Code, w.Header())
				}
			}
			want := 2
			if failure == "missing" {
				want = 1
			}
			if len(calls) != want {
				t.Fatal("请求顺序或成功缓存不符合预期", calls)
			}
		})
	}
}

func TestCustomCoverOriginalWinsAndDisableRestoresRedirect(t *testing.T) {
	f := newDirectoryTestServer(t)
	enableCustomCover(t, f, settings.DefaultCustomCoverURL)
	picture := customCoverPNG(t)
	in := music.FromMap(map[string]any{"source": "wy", "songmid": "cover", "name": "歌曲", "img": "https://original.example/cover"})
	f.server.Catalog.Cache([]*music.Info{in})
	calls := 0
	f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "original.example" {
			t.Fatal("正常原图不应访问备用接口")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	r := httptest.NewRequest("GET", "/rest/getCoverArt?id="+in.TrackID(), nil)
	r = r.WithContext(withUser(r.Context(), f.user))
	w := httptest.NewRecorder()
	f.server.getCoverArt(w, r)
	if w.Code != 200 || calls != 1 {
		t.Fatal("原图成功路径异常", w.Code, calls)
	}
	enableCustomCover(t, f, "")
	w = httptest.NewRecorder()
	f.server.getCoverArt(w, r)
	if w.Code != 302 || calls != 1 || w.Header().Get("Location") != in.Img() {
		t.Fatal("关闭备用接口后应恢复原有封面模式", w.Code)
	}
}

func TestCustomCoverJSONReferencesAndUnsafeTargets(t *testing.T) {
	for _, target := range []string{"https://image.example/cover", "http://127.0.0.1/private"} {
		f := newDirectoryTestServer(t)
		calls := 0
		picture := customCoverPNG(t)
		f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host == "custom.example" {
				body, _ := json.Marshal(map[string]any{"data": map[string]string{"url": target}})
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, nil
			}
			if r.URL.Host != "image.example" {
				t.Fatal("访问了内部网络目标")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
		})}
		r := httptest.NewRequest("GET", "/rest/getCoverArt", nil)
		r = r.WithContext(withUser(r.Context(), f.user))
		w := httptest.NewRecorder()
		f.server.coverWithFallback(w, r, "", "https://custom.example/cover", map[string]string{"title": "歌曲"})
		if strings.Contains(target, "127.0.0.1") {
			if w.Code != 502 || calls != 1 || strings.Contains(w.Body.String(), target) {
				t.Fatal("没有安全拒绝 JSON 中的私网图片地址", w.Code, calls)
			}
		} else if w.Code != 200 || calls != 2 || !bytes.Equal(w.Body.Bytes(), picture) {
			t.Fatal("JSON 图片地址没有被正确获取", w.Code, calls)
		}
	}
}

func TestCustomCoverAlbumMetadataNeedsNoTrackScan(t *testing.T) {
	f := newDirectoryTestServer(t)
	enableCustomCover(t, f, settings.DefaultCustomCoverURL)
	picture := customCoverPNG(t)
	id := music.OnlineAlbumID("wy", "album", "专辑名称", "歌手名称", "")
	f.server.Catalog.SetRequestObserver(func(music.RemoteRequest) { t.Error("封面不应扫描专辑歌曲") })
	f.server.coverHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if q.Has("title") || q.Get("album") != "专辑名称" || q.Get("artist") != "歌手名称" {
			t.Fatal("专辑封面应只传专辑与歌手")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	r := httptest.NewRequest("GET", "/rest/getCoverArt?id="+id, nil)
	r = r.WithContext(withUser(r.Context(), f.user))
	w := httptest.NewRecorder()
	f.server.getCoverArt(w, r)
	if w.Code != 200 {
		t.Fatal("缺少原图的已知专辑应使用备用封面", w.Code)
	}
}
