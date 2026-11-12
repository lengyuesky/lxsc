package fnmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/mediastream"
)

func TestDownloadUsesRealMediaPolicyWithoutPlaybackSideEffects(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	_, body := attachMedia(t, f)
	token := login(t, f, "alice")
	search := searchRequest(t, f, token, "track", "remote", "")
	guid := search["list"].([]any)[0].(map[string]any)["guid"].(string)
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		value, _ := json.Marshal(mode)
		_, _ = f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": value})
		w := request(f, http.MethodGet, "/music/api/v1/download/track?guid="+guid+"&proxy=1&quality=flac24bit&u=admin", token, nil)
		if mode == "proxy" {
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
				t.Fatal("代理下载没有交付真实完整音频", w.Code)
			}
		} else if w.Code != http.StatusFound || w.Header().Get("Location") == "" {
			t.Fatal("客户端覆盖了管理员交付方式", mode, w.Code)
		}
		if _, err := f.db.GetTrack(context.Background(), "tr-wy-remote-one"); !errors.Is(err, db.ErrNotFound) {
			t.Fatal("下载保存了播放元数据", err)
		}
	}
	stars, _ := f.db.ListStarred(context.Background(), f.users["alice"].ID, "track")
	recent, _ := f.db.RecentTracks(context.Background(), f.users["alice"].ID, 100)
	if len(stars) != 0 || len(recent) != 0 {
		t.Fatal("下载写入收藏或收听记录")
	}
	if _, _, err := f.db.ResolveClientID(context.Background(), "fnmusic", guid); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("临时下载产生持久访问标识")
	}
	data := payload(t, request(f, http.MethodGet, "/music/api/v1/download/track/detail?trackGUID="+guid, token, nil))
	if data["track"].(map[string]any)["guid"] != guid || data["audioSpec"] == nil {
		t.Fatal("下载详情没有保持原曲GUID及元数据")
	}
}

func TestDownloadValidRangeAndHeadKeepRealLengths(t *testing.T) {
	f := newFixture(t)
	_, body := attachMedia(t, f)
	token := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	_, _ = f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)})
	path := "/music/api/v1/download/track?guid=" + identifier("track", "tr-wy-one")
	w := request(f, http.MethodHead, path, token, nil)
	if w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatal("下载HEAD没有真实完整长度")
	}
	w = request(f, http.MethodGet, path, token, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatal("初次下载没有保存可续传表示")
	}
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", token)
	r.Header.Set("Range", "bytes=5-9")
	r.Header.Set("If-Range", "not-the-current-validator")
	w = httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Fatal("失效续传必须中止当前文件响应")
			}
		}()
		f.routes.ServeHTTP(w, r)
	}()
	if w.Code < 400 || w.Body.Len() != 0 {
		t.Fatal("失效的If-Range不应拼接部分音频", w.Code)
	}
	r.Header.Set("If-Range", `"fnmusic-test-audio-v1"`)
	w = httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), body[5:10]) || !strings.HasPrefix(w.Header().Get("Content-Range"), "bytes 5-9/") {
		t.Fatal("有效校验条件没有安全透传范围", w.Code)
	}
	r.Header.Del("If-Range")
	w = httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), body[5:10]) {
		t.Fatal("同会话保存的实体校验条件没有用于安全续传")
	}
}

func TestDownloadFailureAbortsInsteadOfReturningFakeAudio(t *testing.T) {
	f := newFixture(t)
	attachMedia(t, f)
	token := login(t, f, "alice")
	newSession := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	server := httptest.NewServer(f.routes)
	t.Cleanup(server.Close)
	for _, item := range []struct{ path, token, rangeValue string }{
		{"/music/api/v1/download/track?guid=" + strings.Repeat("0", 32), token, ""},
		{"/music/api/v1/download/track?guid=" + identifier("track", "tr-wy-one"), "", ""},
		{"/music/api/v1/download/track?guid=" + identifier("track", "tr-wy-one"), newSession, "bytes=100-"},
	} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+item.path, nil)
		if item.token != "" {
			req.Header.Set("Authorization", item.token)
		}
		if item.rangeValue != "" {
			req.Header.Set("Range", item.rangeValue)
		}
		response, err := server.Client().Do(req)
		if response != nil {
			data, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err == nil && readErr == nil {
				t.Fatal("下载失败仍作为完整HTTP文件响应结束", response.StatusCode, string(data))
			}
		}
		if err == nil && response == nil {
			t.Fatal("失败下载没有传输错误")
		}
	}
}

func TestDownloadResumeDescriptionIsSessionScopedAndReportsActualFile(t *testing.T) {
	ctx := context.WithValue(context.Background(), sessionKey{}, [32]byte{1})
	other := context.WithValue(context.Background(), sessionKey{}, [32]byte{2})
	var store downloadResumeStore
	description := mediastream.MediaDescription{ETag: `"file-v1"`, ContentType: "audio/flac", Length: 12345, KnownLength: true}
	store.remember(ctx, "guid", description)
	if store.validator(ctx, "guid") != description.ETag || store.validator(other, "guid") != "" {
		t.Fatal("下载续传条件泄漏到其他会话")
	}
	f := newFixture(t)
	f.server.downloads.remember(ctx, "guid", description)
	view := map[string]any{"audioSpec": map[string]any{"format": "mp3", "codec": "mp3", "bitrate": 320000}}
	f.server.downloadAudioSpec(ctx, "guid", view)
	spec := view["audioSpec"].(map[string]any)
	if spec["size"] != int64(12345) || spec["format"] != "flac" || spec["path"] != "/online/guid.flac" || spec["codec"] != nil || spec["bitrate"] != nil {
		t.Fatal("下载详情没有以实际文件表示更新属性", spec)
	}
	store.mu.Lock()
	key := downloadResumeKey{[32]byte{1}, "guid"}
	entry, _ := store.entries.Get(key)
	entry.expires = time.Now().Add(-time.Second)
	store.entries.Add(key, entry)
	store.mu.Unlock()
	if store.validator(ctx, "guid") != "" {
		t.Fatal("过期实体校验条件仍可续传")
	}
}

func TestChangedOrConcurrentDownloadDescriptionDisablesAutomaticResume(t *testing.T) {
	ctx := context.WithValue(context.Background(), sessionKey{}, [32]byte{1})
	var store downloadResumeStore
	first := mediastream.MediaDescription{ETag: `"same-validator"`, ContentType: "audio/wav", Length: 1000, KnownLength: true, ResourceDigest: [32]byte{1}}
	store.remember(ctx, "guid", first)
	baseline, _ := store.get(ctx, "guid")
	withDate := first
	withDate.LastModified = "Sun, 27 Oct 2024 03:33:20 GMT"
	if !store.matches(ctx, "guid", baseline, withDate) {
		t.Fatal("同一强ETag表示不能因补充Last-Modified而拒绝续传")
	}
	other := first
	other.ResourceDigest = [32]byte{2}
	if store.matches(ctx, "guid", baseline, other) {
		t.Fatal("同校验值的不同媒体地址可拼接续传")
	}
	store.remember(ctx, "guid", other)
	if store.validator(ctx, "guid") != "" || store.matches(ctx, "guid", baseline, first) {
		t.Fatal("并发不同表示没有禁止自动续传")
	}
}
