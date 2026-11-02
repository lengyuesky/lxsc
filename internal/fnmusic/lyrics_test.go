package fnmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/js"
	"lxsc/internal/metrics"
	"lxsc/internal/music"
)

func lyricSDK(t *testing.T, f *fixture, bundle string) *js.SDKPool {
	t.Helper()
	pool, err := js.NewSDKPool(2, "", bundle, &http.Client{}, &http.Client{}, f.server.Catalog.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f.server.Catalog.SDK = pool
	return pool
}

func lyricSDKResult(t *testing.T, f *fixture, result js.LyricResult) *js.SDKPool {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return lyricSDK(t, f, "globalThis.__sdk_call = () => ("+string(raw)+");")
}

func lyricPayload(t *testing.T, w *httptest.ResponseRecorder) lyricData {
	t.Helper()
	_ = payload(t, w)
	var envelope struct {
		Data lyricData `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func sdkCalls(pool *js.SDKPool) int64 {
	return pool.Performance()["calls"].(metrics.Snapshot).Count
}

func trackHasLyric(t *testing.T, f *fixture, token, guid string) bool {
	t.Helper()
	data := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	for _, value := range data["list"].([]any) {
		track := value.(map[string]any)
		if track["guid"] == guid {
			return track["hasLyric"].(bool)
		}
	}
	t.Fatal("track missing from authorized list")
	return false
}

func TestLyricsWireContractPreservesTimingTranslationAndOffset(t *testing.T) {
	for _, offset := range []int{125, -250} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			f := newFixture(t)
			token := login(t, f, "alice")
			pool := lyricSDKResult(t, f, js.LyricResult{
				Lyric:  fmt.Sprintf("[offset:%d]\n[00:01.234]原文第一行\n[00:02.345]原文第二行", offset),
				TLyric: "[00:01.234]译文第一行\n[00:02.345]译文第二行",
				RLyric: "[00:01.234]roma one\n[00:02.345]roma two",
			})
			guid := identifier("track", "tr-wy-one")
			if trackHasLyric(t, f, token, guid) || sdkCalls(pool) != 0 {
				t.Fatal("lists must not fetch or promise unknown lyrics")
			}
			for range 2 {
				data := lyricPayload(t, request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, token, nil))
				if len(data.List) != 1 || data.Preferred != data.List[0].GUID || !validGUID(data.Preferred) {
					t.Fatal("preferred must identify a real lyric entry")
				}
				item := data.List[0]
				if !item.IsLRC || item.Offset != offset || strings.Contains(item.Content, "[offset:") {
					t.Fatal("offset was lost or duplicated in lyric content", item.Offset)
				}
				lines, contentOffset, synced := music.ParseLRC(item.Content)
				if !synced || contentOffset != 0 || len(lines) != 6 || lines[0].Start != 1234 || lines[3].Start != 2345 || lines[1].Value != "译文第一行" || lines[2].Value != "roma one" {
					t.Fatal("lyric serialization lost milliseconds or supplementary text")
				}
			}
			if sdkCalls(pool) != 1 || !trackHasLyric(t, f, token, guid) {
				t.Fatal("valid lyrics were not reused by the track availability flag")
			}
			track, _ := f.server.Catalog.LocalTrack(context.Background(), "tr-wy-one")
			cached, ok := f.server.Catalog.CachedLyric(track)
			if !ok || cached.Offset != offset || len(cached.Lines) != 2 || !strings.Contains(cached.Raw, "[offset:") {
				t.Fatal("protocol serialization mutated shared lyrics")
			}
		})
	}
}

func TestLyricsPlainTextAndEmptyResultsRemainTruthful(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	guid := identifier("track", "tr-wy-one")
	_ = trackHasLyric(t, f, token, guid)
	empty := lyricPayload(t, request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, token, nil))
	if empty.List == nil || len(empty.List) != 0 || empty.Preferred != "" || trackHasLyric(t, f, token, guid) {
		t.Fatal("unavailable lyrics were marked present")
	}
	lyricSDKResult(t, f, js.LyricResult{Lyric: "[ti:metadata only]\n[00:01.00]"})
	empty = lyricPayload(t, request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, token, nil))
	if len(empty.List) != 0 || trackHasLyric(t, f, token, guid) {
		t.Fatal("blank timed lyrics were treated as usable")
	}
	pool := lyricSDKResult(t, f, js.LyricResult{Lyric: "没有时间轴的第一行\n没有时间轴的第二行"})
	data := lyricPayload(t, request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, token, nil))
	if len(data.List) != 1 || data.List[0].IsLRC || data.List[0].Content != "没有时间轴的第一行\n没有时间轴的第二行" || sdkCalls(pool) != 1 {
		t.Fatal("empty results prevented retry or plain lyrics acquired a fake timeline")
	}
}

func TestLyricsMetadataLoadsOnlyCurrentTrackWithBoundedWait(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	pool := lyricSDKResult(t, f, js.LyricResult{Lyric: "[00:01.00]metadata lyric"})
	guid := identifier("track", "tr-wy-one")
	_ = trackHasLyric(t, f, token, guid)
	if sdkCalls(pool) != 0 {
		t.Fatal("track list performed upstream lyric work")
	}
	data := payload(t, request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, token, nil))
	if data["track"].(map[string]any)["hasLyric"] != true || sdkCalls(pool) != 1 {
		t.Fatal("current-track metadata did not lazily expose available lyrics")
	}
	if trackHasLyric(t, f, token, identifier("track", "tr-wy-public")) {
		t.Fatal("current-track load promised lyrics for unrelated tracks")
	}
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"customLyricsURL": json.RawMessage(`"https://changed.example/lyrics"`)}); err != nil {
		t.Fatal(err)
	}
	if trackHasLyric(t, f, token, guid) {
		t.Fatal("changed fallback configuration reused an old availability flag")
	}
	lyricSDK(t, f, `globalThis.__sdk_call = () => new Promise(() => {});`)
	started := time.Now()
	data = payload(t, request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, token, nil))
	if time.Since(started) > 4*time.Second || data["track"].(map[string]any)["hasLyric"] != false {
		t.Fatal("slow lyrics blocked metadata or acquired a fabricated availability flag")
	}
}

func TestLyricsUseExistingSourceFallback(t *testing.T) {
	f := newFixture(t)
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	sources := js.NewSourceManager(string(prelude), client, client, f.server.Catalog.Log)
	t.Cleanup(sources.UnloadAll)
	script := `lx.on(lx.EVENT_NAMES.request, () => Promise.resolve({lyric:'[00:01.000]source lyric'}));
lx.send(lx.EVENT_NAMES.inited, {status:true, sources:{wy:{name:'synthetic',type:'music',actions:['lyric'],qualitys:[]}}});`
	if _, err := sources.Load(context.Background(), 1, script); err != nil {
		t.Fatal(err)
	}
	f.server.Catalog.Sources = sources
	token := login(t, f, "alice")
	guid := identifier("track", "tr-wy-one")
	_ = trackHasLyric(t, f, token, guid)
	data := lyricPayload(t, request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, token, nil))
	if len(data.List) != 1 || !strings.Contains(data.List[0].Content, "source lyric") {
		t.Fatal("protocol skipped the shared catalog's source fallback")
	}
}

func TestLyricsCacheDoesNotAuthorizeOtherUsersOrRevokedTracks(t *testing.T) {
	f := newFixture(t)
	alice, bob := login(t, f, "alice"), login(t, f, "bob")
	identifyCovers(t, f, alice)
	identifyCovers(t, f, bob)
	pool := lyricSDKResult(t, f, js.LyricResult{Lyric: "[00:01.00]private synthetic lyric"})
	for _, key := range []string{"two", "public"} {
		data := lyricPayload(t, request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+identifier("track", "tr-wy-"+key), bob, nil))
		if len(data.List) != 1 {
			t.Fatal("owner lyric setup failed")
		}
	}
	no := false
	if err := f.db.UpdatePlaylistMeta(context.Background(), "pl-public", nil, nil, &no); err != nil {
		t.Fatal(err)
	}
	before := sdkCalls(pool)
	for _, guid := range []string{identifier("track", "tr-wy-two"), identifier("track", "tr-wy-public"), strings.Repeat("0", 32), "invalid"} {
		w := request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, alice, nil)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "private synthetic lyric") {
			t.Fatal("shared lyric cache bypassed current authorization", w.Code)
		}
	}
	if sdkCalls(pool) != before {
		t.Fatal("denied lyrics reached the platform")
	}
}

func TestLyricsConcurrentRequestsSharePlatformWork(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	guid := identifier("track", "tr-wy-one")
	_ = trackHasLyric(t, f, token, guid)
	pool := lyricSDK(t, f, `globalThis.__sdk_call = () => new Promise(resolve => setTimeout(() => resolve({lyric:'[00:01.00]shared lyric'}), 250));`)
	results := make(chan *httptest.ResponseRecorder, 6)
	for range 6 {
		go func() {
			results <- request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, token, nil)
		}()
	}
	for range 6 {
		select {
		case w := <-results:
			if data := lyricPayload(t, w); len(data.List) != 1 {
				t.Fatal("shared platform request did not deliver lyrics")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent lyrics did not finish")
		}
	}
	if sdkCalls(pool) != 1 || f.server.Catalog.RequestLimits.Stats().Active != 0 {
		t.Fatal("lyrics repeated shared work or leaked request permits")
	}
}

func TestLyricsFlushFailureDoesNotLogContent(t *testing.T) {
	f := newFixture(t)
	token := login(t, f, "alice")
	guid := identifier("track", "tr-wy-one")
	_ = trackHasLyric(t, f, token, guid)
	lyricSDKResult(t, f, js.LyricResult{Lyric: "[00:01.00]敏感合成歌词正文"})
	r := httptest.NewRequest(http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, nil)
	r.AddCookie(&http.Cookie{Name: "music-token", Value: token})
	w := &resourceFlushWriter{ResponseRecorder: httptest.NewRecorder()}
	f.routes.ServeHTTP(w, r)
	events := f.server.Diagnostics.List()
	last := events[len(events)-1]
	if last.Result != "failed" || last.Error != "write_error" || last.Format != "json" || last.Lines == nil || *last.Lines != 1 {
		t.Fatal("lyric delivery failure was misreported", last)
	}
	raw, _ := json.Marshal(events)
	for _, secret := range []string{token, "alice", "敏感合成歌词正文", "synthetic private network message"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("lyric diagnostics include content or credentials", secret)
		}
	}
}
