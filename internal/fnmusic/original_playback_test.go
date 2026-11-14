package fnmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/js"
	"lxsc/internal/mediastream"
	"lxsc/internal/music"
	"lxsc/internal/testaudio"
)

func originalPlaylistMedia(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" || !strings.HasPrefix(w.Body.String(), "#EXTM3U\n") || !strings.HasSuffix(w.Body.String(), "#EXT-X-ENDLIST\n") {
		t.Fatalf("未返回完整原始播放清单：HTTP %d，%s", w.Code, w.Body.String())
	}
	var media []string
	for line := range strings.SplitSeq(w.Body.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			media = append(media, line)
		}
	}
	if len(media) != 1 {
		t.Fatalf("原始播放清单应只指向一份完整音频：%v", media)
	}
	return media[0]
}

func TestStandardPlaybackUsesOriginalMedia(t *testing.T) {
	for _, audio := range []struct {
		format, mime, quality string
		body                  []byte
	}{
		{"wav", "audio/wav", "320k", testaudio.WAV()},
		{"mp3", "audio/mpeg", "320k", testaudio.MP3},
		{"flac", "audio/flac", "flac", testaudio.FLAC},
	} {
		t.Run(audio.format, func(t *testing.T) {
			f := newFixture(t)
			// 内嵌测试音频约一秒钟，与曲目时长保持一致以验证完整原文件交付。
			track := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "one", "singer": "测试歌手", "albumName": "测试专辑", "interval": "00:01"})
			if err := f.server.Catalog.RememberSync(context.Background(), []*music.Info{track}); err != nil {
				t.Fatal(err)
			}
			var sent atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("原始音频上游收到了飞牛会话")
				}
				if r.URL.Query().Get("quality") != audio.quality {
					t.Error("标准音质请求改变了用户的原始取链音质")
				}
				w.Header().Set("Content-Type", audio.mime)
				http.ServeContent(&byteCounter{ResponseWriter: w, total: &sent}, r, "tone."+audio.format, time.Unix(0, 0), bytes.NewReader(audio.body))
			}))
			t.Cleanup(upstream.Close)
			prelude, err := assets.JS.ReadFile("js/prelude.js")
			if err != nil {
				t.Fatal(err)
			}
			sources := js.NewSourceManager(string(prelude), upstream.Client(), upstream.Client(), f.server.Catalog.Log)
			t.Cleanup(sources.UnloadAll)
			script := fmt.Sprintf(`lx.on(lx.EVENT_NAMES.request, ({info}) => Promise.resolve(%q+'?quality='+info.type));
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{name:'test',type:'music',actions:['musicUrl'],qualitys:[%q]}}});`, upstream.URL+"/tone."+audio.format, audio.quality)
			if _, err := sources.Load(context.Background(), 1, script); err != nil {
				t.Fatal(err)
			}
			f.server.Catalog.Sources = sources
			f.server.Catalog.AudioCheck = nil
			f.server.Media = &mediastream.Service{Catalog: f.server.Catalog, Settings: f.server.Settings, Log: f.server.Catalog.Log, HTTP: upstream.Client(), Diagnostics: &diagnostics.Events{}}
			if err := f.db.UpdateUserQuality(context.Background(), f.users["alice"].ID, audio.quality); err != nil {
				t.Fatal(err)
			}
			token := login(t, f, "alice")
			_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
			guid := identifier("track", "tr-wy-one")
			body := map[string]any{"guid": guid, "lan": false, "output": map[string]any{"start": 30, "codec": "opus", "bitrate": 128}, "proxy": 1, "quality": "128k"}
			prepared := payload(t, request(f, http.MethodPost, "/music/api/v1/track/transcode", token, body))
			if prepared["guid"] != guid || prepared["status"] != "ready" || prepared["transcoded"] != false {
				t.Fatalf("标准播放没有明确准备原始音频：%v", prepared)
			}
			_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/transcode?guid="+guid+"&quality=128k", token, nil))
			playlistPath := "/music/api/v1/track/hls/" + guid + "/preset.m3u8"
			playlist := request(f, http.MethodGet, playlistPath+"?quality=128k&proxy=1", token, nil)
			media := originalPlaylistMedia(t, playlist)
			if media != "/music/api/v1/track/stream?guid="+guid || strings.Contains(playlist.Body.String(), token) || playlist.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("播放清单泄漏会话或允许客户端覆盖原始音质/播放模式")
			}
			for _, control := range []string{"heartbeat", "quit"} {
				_ = payload(t, request(f, http.MethodPost, "/music/api/v1/track/transcode/"+control, token, map[string]any{"guid": guid, "timestamp": 30}))
			}
			if sent.Load() != 0 {
				t.Fatal("准备、清单或心跳提前下载了音频")
			}
			page, err := f.db.RecentPlayback(context.Background(), db.PlaybackQuery{UserID: f.users["alice"].ID}, time.Now())
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("准备、清单或心跳被记成播放：%+v %v", page, err)
			}
			for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
				value, _ := json.Marshal(mode)
				if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": value}); err != nil {
					t.Fatal(err)
				}
				w := request(f, http.MethodGet, media+"&quality=128k&proxy=1", token, nil)
				if mode == "proxy" {
					if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), audio.body) {
						t.Fatalf("代理改变了原始 %s 音频字节：HTTP %d", audio.format, w.Code)
					}
				} else {
					location, err := url.Parse(w.Header().Get("Location"))
					if w.Code != http.StatusFound || err != nil || location.Query().Get("quality") != audio.quality {
						t.Fatalf("%s 没有沿用原始取链音质：HTTP %d", mode, w.Code)
					}
					response, err := upstream.Client().Get(location.String())
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if err != nil || !bytes.Equal(got, audio.body) {
						t.Fatal("直链没有交付完整原始音频", err)
					}
				}
			}
			stats, err := f.db.ListeningStatistics(context.Background(), f.users["alice"].ID, 30, time.Now())
			if err != nil || stats.Plays != 0 {
				t.Fatal("标准播放的准备、心跳或媒体请求增加了收听次数", err)
			}
		})
	}
}

func TestStandardPlaybackHeadAndRange(t *testing.T) {
	f := newFixture(t)
	_, audio := attachMedia(t, f)
	token := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	guid := identifier("track", "tr-wy-one")
	path := "/music/api/v1/track/hls/" + guid + "/preset.m3u8"
	playlist := request(f, http.MethodGet, path, token, nil)
	media := originalPlaylistMedia(t, playlist)
	head := request(f, http.MethodHead, path, token, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(playlist.Body.Len()) {
		t.Fatal("清单 HEAD 未返回正确长度或包含正文")
	}
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)}); err != nil {
		t.Fatal(err)
	}
	head = request(f, http.MethodHead, media, token, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len(audio)) {
		t.Fatal("标准播放未沿用原始媒体 HEAD")
	}
	r := httptest.NewRequest(http.MethodGet, media, nil)
	r.Header.Set("Authorization", token)
	r.Header.Set("Range", "bytes=5-9")
	r.Header.Set("If-Range", `"fnmusic-test-audio-v1"`)
	w := httptest.NewRecorder()
	f.routes.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), audio[5:10]) || w.Header().Get("Content-Range") != fmt.Sprintf("bytes 5-9/%d", len(audio)) {
		t.Fatal("标准播放没有保留原始文件 Range/If-Range")
	}
}

func TestStandardPlaybackPermissionsAndRevocation(t *testing.T) {
	f := newFixture(t)
	attachMedia(t, f)
	token, other := login(t, f, "alice"), login(t, f, "bob")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	check := func(guid, session string, status int) {
		t.Helper()
		for _, endpoint := range []string{"transcode", "transcode/heartbeat", "transcode/quit"} {
			if got := request(f, http.MethodPost, "/music/api/v1/track/"+endpoint, session, map[string]string{"guid": guid}).Code; got != status {
				t.Fatalf("%s 应返回 %d，实际 %d", endpoint, status, got)
			}
		}
		if got := request(f, http.MethodGet, "/music/api/v1/track/transcode?guid="+guid, session, nil).Code; got != status {
			t.Fatalf("GET 准备应返回 %d，实际 %d", status, got)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if got := request(f, method, "/music/api/v1/track/hls/"+guid+"/preset.m3u8", session, nil).Code; got != status {
				t.Fatalf("%s 清单应返回 %d，实际 %d", method, status, got)
			}
		}
	}
	private := identifier("track", "tr-wy-one")
	check(private, "", http.StatusUnauthorized)
	check(private, other, http.StatusNotFound)
	check(strings.Repeat("0", 32), token, http.StatusNotFound)
	public := identifier("track", "tr-wy-public")
	check(public, token, http.StatusOK)
	no := false
	if err := f.db.UpdatePlaylistMeta(context.Background(), "pl-public", nil, nil, &no); err != nil {
		t.Fatal(err)
	}
	check(public, token, http.StatusNotFound)
	if w := request(f, http.MethodGet, "/music/api/v1/track/stream?guid="+public, token, nil); w.Code != http.StatusNotFound {
		t.Fatal("旧清单指向的音频绕过了撤销权限")
	}
	_ = payload(t, request(f, http.MethodPost, "/music/api/v1/user/logout", token, nil))
	check(private, token, http.StatusUnauthorized)
}

func TestStandardPlaybackSearchGrantIsSessionScoped(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	lyricSDK(t, f, strings.ReplaceAll(searchTestSDK, "songmid:'remote-", "interval:'00:45',songmid:'remote-"))
	attachMedia(t, f)
	token, other := login(t, f, "alice"), login(t, f, "bob")
	data := searchRequest(t, f, token, "track", "remote", "")
	guid := data["list"].([]any)[0].(map[string]any)["guid"].(string)
	path := "/music/api/v1/track/hls/" + guid + "/preset.m3u8"
	_ = payload(t, request(f, http.MethodPost, "/music/api/v1/track/transcode", token, map[string]string{"guid": guid}))
	_ = originalPlaylistMedia(t, request(f, http.MethodGet, path, token, nil))
	if w := request(f, http.MethodGet, path, other, nil); w.Code != http.StatusNotFound {
		t.Fatal("其他会话通过曲目缓存读取了搜索结果清单")
	}
	f.server.searches.mu.Lock()
	for _, key := range f.server.searches.entries.Keys() {
		entry, _ := f.server.searches.entries.Peek(key)
		entry.expires = time.Now().Add(-time.Second)
		f.server.searches.entries.Add(key, entry)
	}
	f.server.searches.mu.Unlock()
	if w := request(f, http.MethodGet, path, token, nil); w.Code != http.StatusNotFound {
		t.Fatal("搜索授权过期后旧清单仍可访问")
	}
}

func TestStandardPlaybackRejectsInvalidPreparation(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	guid := identifier("track", "tr-wy-one")
	body, _ := json.Marshal(map[string]string{"guid": guid})
	for _, item := range []struct {
		body, contentType, origin string
		status                    int
	}{
		{"{", "application/json", "", http.StatusBadRequest},
		{string(body) + "{}", "application/json", "", http.StatusBadRequest},
		{string(body), "text/plain", "", http.StatusUnsupportedMediaType},
		{string(body), "application/json", "https://other.example", http.StatusForbidden},
		{`{"guid":"` + guid + `","padding":"` + strings.Repeat("x", 64<<10) + `"}`, "application/json", "", http.StatusBadRequest},
	} {
		r := httptest.NewRequest(http.MethodPost, "/music/api/v1/track/transcode", strings.NewReader(item.body))
		r.Header.Set("Authorization", token)
		r.Header.Set("Content-Type", item.contentType)
		if item.origin != "" {
			r.Header.Set("Origin", item.origin)
		}
		w := httptest.NewRecorder()
		f.routes.ServeHTTP(w, r)
		if w.Code != item.status {
			t.Fatalf("无效准备应返回 %d，实际 %d", item.status, w.Code)
		}
	}
	if w := request(f, http.MethodPost, "/music/api/v1/track/transcode", token, map[string]string{"guid": guid}); w.Code != http.StatusServiceUnavailable {
		t.Fatal("媒体未配置仍声称原始播放已就绪")
	}
	attachMedia(t, f)
	unknown := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "未知时长"})
	if err := f.server.Catalog.RememberSync(context.Background(), []*music.Info{unknown}); err != nil {
		t.Fatal(err)
	}
	if w := request(f, http.MethodPost, "/music/api/v1/track/transcode", token, map[string]string{"guid": guid}); w.Code != http.StatusUnprocessableEntity {
		t.Fatal("未知时长被包装成可用的播放清单")
	}
	if w := request(f, http.MethodGet, "/music/api/v1/track/hls/"+guid+"/preset.m3u8", token, nil); w.Code != http.StatusUnprocessableEntity {
		t.Fatal("未知时长的清单伪造了歌曲时长")
	}
	if w := request(f, http.MethodGet, "/music/api/v1/track/stream?guid="+guid, token, nil); w.Code != http.StatusFound {
		t.Fatal("未知时长歌曲的原始播放受到影响")
	}
}
