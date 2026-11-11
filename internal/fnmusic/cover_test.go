package fnmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
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
	"lxsc/internal/coverart"
	"lxsc/internal/music"
	"lxsc/internal/settings"
	"lxsc/internal/subsonic"
)

type resourceTransport func(*http.Request) (*http.Response, error)

func (f resourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func coverPNG(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, 3, 3))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func imageResponse(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
}

func coverFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	tracks := make([]*music.Info, 0, 3)
	for _, key := range []string{"one", "two", "public"} {
		tracks = append(tracks, music.FromMap(map[string]any{
			"source": "wy", "songmid": key, "name": "synthetic-title-" + key,
			"singer": "synthetic-artist-" + key, "albumName": "synthetic-album-" + key,
			"img": "https://image.example/" + key,
		}))
	}
	if err := f.server.Catalog.RememberSync(context.Background(), tracks); err != nil {
		t.Fatal(err)
	}
	return f
}

func identifyCovers(t *testing.T, f *fixture, token string) {
	t.Helper()
	for _, path := range []string{"track/list", "playlist/list"} {
		_ = payload(t, request(f, http.MethodGet, "/music/api/v1/"+path, token, nil))
	}
}

func trackCoverIDs(t *testing.T, f *fixture, key, playlist string) []string {
	t.Helper()
	track, err := f.server.Catalog.LocalTrack(context.Background(), "tr-wy-"+key)
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		"track_" + identifier("track", track.TrackID()),
		"playlist_" + identifier("playlist", playlist),
		"album_" + identifier("album", track.AlbumSubID()),
		"artist_" + identifier("artist", track.ArtistSubID()),
	}
}

func TestCoverFourKindsReturnRealImageAndShareCache(t *testing.T) {
	f := coverFixture(t)
	token := login(t, f, "alice")
	identifyCovers(t, f, token)
	picture := coverPNG(t)
	var calls atomic.Int32
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Host != "image.example" || r.URL.Path != "/one" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" {
			t.Error("cover delivery changed the resource or forwarded client credentials")
		}
		return imageResponse(http.StatusOK, picture), nil
	})}
	for _, id := range trackCoverIDs(t, f, "one", "pl-alice") {
		for _, base := range []string{"/music/api/v1/static/cover", "/music/static/cover"} {
			w := request(f, http.MethodGet, base+"?coverId="+id, token, nil)
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Location") != "" {
				t.Fatal("authorized cover did not return validated image bytes", id, w.Code)
			}
			if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls.Load() != 1 {
		t.Fatal("protocol resource kinds did not share the same image cache", calls.Load())
	}
	coverEvents := 0
	for _, event := range f.server.Diagnostics.List() {
		if event.Endpoint != "getCoverArt" {
			continue
		}
		coverEvents++
		if event.CoverOrigin != "original" || event.Result != "ok" || event.BytesWritten == nil || event.ResponseBytes == nil || *event.BytesWritten != int64(len(picture)) || *event.BytesWritten != *event.ResponseBytes {
			t.Fatal("cover diagnostic lost origin or delivery counts", event)
		}
	}
	if coverEvents != 8 {
		t.Fatalf("cover diagnostics omitted requests: got %d, want 8", coverEvents)
	}
}

func TestCoverPresentationVersionChangesWithoutChangingResourceGUID(t *testing.T) {
	f := coverFixture(t)
	token := login(t, f, "alice")
	identifyCovers(t, f, token)
	track, _ := f.server.Catalog.LocalTrack(context.Background(), "tr-wy-one")
	guid := identifier("track", track.TrackID())
	view, err := f.server.trackView(context.Background(), track, false)
	if err != nil {
		t.Fatal(err)
	}
	first := view["coverId"].(string)
	if kind, parsed, ok := parseCoverID(first); !ok || kind != "track" || parsed != guid || first == "track_"+guid {
		t.Fatal("cover revision changed identity or left old placeholders cacheable")
	}
	picture := coverPNG(t)
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(*http.Request) (*http.Response, error) {
		return imageResponse(http.StatusOK, picture), nil
	})}
	for _, id := range []string{first, "track_" + guid} {
		w := request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+id, token, nil)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) {
			t.Fatal("current or legacy cover ID no longer resolves")
		}
	}
	changed := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": track.Name(), "singer": track.Singer(), "albumName": track.Album(), "img": "https://changed-image.example/cover"})
	if err := f.server.Catalog.RememberSync(context.Background(), []*music.Info{changed}); err != nil {
		t.Fatal(err)
	}
	second := f.server.coverID("track", guid, changed)
	if first == second || f.server.coverID("track", guid, changed) != second {
		t.Fatal("metadata revisions are not stable or do not invalidate old artwork")
	}
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"customCoverURL": json.RawMessage(`"https://fallback.example/cover?key=synthetic-private"`)}); err != nil {
		t.Fatal(err)
	}
	third := f.server.coverID("track", guid, changed)
	if second == third || strings.Contains(third, "fallback.example") || strings.Contains(third, "synthetic-private") {
		t.Fatal("cover configuration is not versioned privately")
	}
	for _, id := range []string{first, second, third} {
		if _, parsed, ok := parseCoverID(id); !ok || parsed != guid {
			t.Fatal("presentation version was treated as an authorization GUID")
		}
	}
	for _, revision := range []string{"v0-0123456789ab", "v1-short", "v1-0123456789AB", "v1-0123456789ab_extra"} {
		if _, _, ok := parseCoverID("track_" + guid + "_" + revision); ok {
			t.Fatal("malformed revision accepted", revision)
		}
	}
}

func TestCoverFallbackRecoveryAndResourceMetadata(t *testing.T) {
	f := coverFixture(t)
	token := login(t, f, "alice")
	identifyCovers(t, f, token)
	value, _ := json.Marshal(settings.DefaultCustomCoverURL)
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"customCoverURL": value}); err != nil {
		t.Fatal(err)
	}
	picture := coverPNG(t)
	var restored atomic.Bool
	var customCalls atomic.Int32
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "image.example" {
			return imageResponse(http.StatusForbidden, []byte("<html>not an image</html>")), nil
		}
		if r.URL.Host != "api.lrc.cx" {
			t.Error("unexpected fallback target")
		}
		customCalls.Add(1)
		if !restored.Load() {
			return imageResponse(http.StatusServiceUnavailable, nil), nil
		}
		q := r.URL.Query()
		if q.Get("artist") != "synthetic-artist-one" {
			t.Error("fallback artist metadata was not preserved")
		}
		return imageResponse(http.StatusOK, picture), nil
	})}
	ids := trackCoverIDs(t, f, "one", "pl-alice")
	w := request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+ids[0], token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || bytes.Equal(w.Body.Bytes(), picture) {
		t.Fatal("unavailable authorized image should be temporary and uncached")
	}
	events := f.server.Diagnostics.List()
	if last := events[len(events)-1]; last.Result != "unavailable" || last.CoverOrigin != "placeholder" {
		t.Fatal("placeholder was misreported as a real cover", last)
	}
	restored.Store(true)
	for _, id := range ids {
		w = request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+id, token, nil)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) || strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("temporary image poisoned fallback recovery", id)
		}
	}
	if customCalls.Load() != 4 {
		t.Fatal("album and artist fallback keys must use their own matching fields", customCalls.Load())
	}
	events = f.server.Diagnostics.List()
	if last := events[len(events)-1]; last.Result != "ok" || last.CoverOrigin != "custom" {
		t.Fatal("fallback success was not recorded", last)
	}
}

func TestCoverCacheCannotBypassCurrentPermissions(t *testing.T) {
	f := coverFixture(t)
	alice, bob := login(t, f, "alice"), login(t, f, "bob")
	identifyCovers(t, f, alice)
	identifyCovers(t, f, bob)
	picture := coverPNG(t)
	var calls atomic.Int32
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return imageResponse(http.StatusOK, picture), nil
	})}
	publicIDs := trackCoverIDs(t, f, "public", "pl-public")
	privateIDs := trackCoverIDs(t, f, "two", "pl-bob")
	for _, id := range append(publicIDs, privateIDs...) {
		if request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+id, bob, nil).Code != http.StatusOK {
			t.Fatal("owner cover setup failed")
		}
	}
	no := false
	if err := f.db.UpdatePlaylistMeta(context.Background(), "pl-public", nil, nil, &no); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for _, id := range append(append(publicIDs, privateIDs...), "track_"+strings.Repeat("0", 32), "track_invalid", "unknown_"+identifier("track", "tr-wy-one")) {
		w := request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+id, alice, nil)
		if w.Code != http.StatusNotFound || strings.HasPrefix(w.Header().Get("Content-Type"), "image/") {
			t.Fatal("cached GUID bypassed resource permissions", id, w.Code)
		}
	}
	if calls.Load() != before {
		t.Fatal("denied covers reached upstream")
	}
	if request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+publicIDs[0], "", nil).Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated cover was accepted")
	}
}

func TestCoverUnsafeTargetIsNotHiddenAndEmptyPlaylistNeedsNoUpstream(t *testing.T) {
	f := coverFixture(t)
	token := login(t, f, "alice")
	identifyCovers(t, f, token)
	unsafe := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "synthetic", "img": "http://127.0.0.1/private"})
	if err := f.server.Catalog.RememberSync(context.Background(), []*music.Info{unsafe}); err != nil {
		t.Fatal(err)
	}
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(*http.Request) (*http.Response, error) {
		t.Error("unsafe or empty resource reached upstream")
		return nil, errors.New("must not be requested")
	})}
	w := request(f, http.MethodGet, "/music/api/v1/static/cover?coverId=track_"+identifier("track", unsafe.TrackID()), token, nil)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "127.0.0.1") {
		t.Fatal("unsafe cover was masked by placeholder or leaked its address")
	}
	if err := f.db.CreatePlaylist(context.Background(), "pl-empty", f.users["alice"].ID, "empty", nil); err != nil {
		t.Fatal(err)
	}
	identifyCovers(t, f, token)
	w = request(f, http.MethodGet, "/music/api/v1/static/cover?coverId=playlist_"+identifier("playlist", "pl-empty"), token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("authorized empty playlist should have a local temporary image")
	}
	if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
		t.Fatal(err)
	}
}

func TestCoverSixConcurrentImagesQueueWithoutRejecting(t *testing.T) {
	f := newFixture(t)
	tracks := make([]*music.Info, 6)
	ids := make([]string, 6)
	for i := range tracks {
		tracks[i] = music.FromMap(map[string]any{"source": "wy", "songmid": fmt.Sprint("burst", i), "name": "synthetic", "img": fmt.Sprintf("https://image.example/burst%d", i)})
		ids[i] = tracks[i].TrackID()
	}
	if err := f.server.Catalog.RememberSync(context.Background(), tracks); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreatePlaylist(context.Background(), "pl-burst", f.users["alice"].ID, "burst", ids); err != nil {
		t.Fatal(err)
	}
	token := login(t, f, "alice")
	identifyCovers(t, f, token)
	picture := coverPNG(t)
	started, release := make(chan struct{}, 6), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return imageResponse(http.StatusOK, picture), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	results := make(chan *httptest.ResponseRecorder, 6)
	for _, id := range ids {
		go func() {
			results <- request(f, http.MethodGet, "/music/api/v1/static/cover?coverId=track_"+identifier("track", id), token, nil)
		}()
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("four image requests did not reach upstream")
		}
	}
	deadline := time.Now().Add(time.Second)
	for f.server.Covers.Limits.Stats().Queued != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := f.server.Covers.Limits.Stats(); stats.Active != 4 || stats.Queued != 2 || stats.Rejected != 0 {
		t.Fatal("concurrent covers were rejected instead of queued", stats)
	}
	unblock()
	for range 6 {
		select {
		case w := <-results:
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) {
				t.Fatal("concurrent request did not return a real cover", w.Code)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("queued cover did not finish")
		}
	}
	if calls.Load() != 6 || f.server.Covers.Limits.Stats().Active != 0 {
		t.Fatal("cover requests or permits were lost")
	}
}

func TestCoverServiceSharesCacheWithSubsonic(t *testing.T) {
	f := coverFixture(t)
	token := login(t, f, "alice")
	identifyCovers(t, f, token)
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"coverMode": json.RawMessage(`"proxy"`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateAPIKey(context.Background(), f.users["alice"].ID, "synthetic-cover-key", "test"); err != nil {
		t.Fatal(err)
	}
	picture := coverPNG(t)
	var calls atomic.Int32
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return imageResponse(http.StatusOK, picture), nil
	})}
	sub := &subsonic.Server{DB: f.db, Catalog: f.server.Catalog, Settings: f.server.Settings, Secret: f.box, Covers: f.server.Covers, Diagnostics: f.server.Diagnostics}
	routes := chi.NewRouter()
	routes.Mount("/music", f.server.Routes())
	routes.Mount("/rest", sub.Routes())
	f.routes = routes
	q := url.Values{"id": {"tr-wy-one"}, "apiKey": {"synthetic-cover-key"}, "f": {"json"}}
	w := request(f, http.MethodGet, "/rest/getCoverArt?"+q.Encode(), "", nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) {
		t.Fatal("Subsonic shared cover setup failed")
	}
	w = request(f, http.MethodGet, "/music/api/v1/static/cover?coverId=track_"+identifier("track", "tr-wy-one"), token, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) || calls.Load() != 1 {
		t.Fatal("music protocols did not share the cache")
	}
	events := f.server.Diagnostics.List()
	if last := events[len(events)-1]; last.Client != "fnmusic" || !last.Cached || last.CoverOrigin != "original" {
		t.Fatal("shared cache lost protocol-specific diagnostics", last)
	}
	if f.server.Covers.Performance().(map[string]any)["hits"].(uint64) == 0 {
		t.Fatal("shared performance did not count the other protocol's hit")
	}
}

type resourceFlushWriter struct {
	*httptest.ResponseRecorder
}

func (w *resourceFlushWriter) FlushError() error {
	return errors.New("synthetic private network message")
}

func TestCoverFlushFailureAndDiagnosticPrivacy(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		f := coverFixture(t)
		token := login(t, f, "alice")
		identifyCovers(t, f, token)
		picture := coverPNG(t)
		f.server.Covers = &coverart.Service{HTTP: &http.Client{Transport: resourceTransport(func(*http.Request) (*http.Response, error) {
			return imageResponse(status, picture), nil
		})}}
		r := httptest.NewRequest(http.MethodGet, "/music/api/v1/static/cover?coverId=track_"+identifier("track", "tr-wy-one"), nil)
		r.AddCookie(&http.Cookie{Name: "music-token", Value: token})
		w := &resourceFlushWriter{ResponseRecorder: httptest.NewRecorder()}
		f.routes.ServeHTTP(w, r)
		events := f.server.Diagnostics.List()
		last := events[len(events)-1]
		if last.Result != "failed" || last.Error != "write_error" || last.BytesWritten == nil || last.ResponseBytes == nil || *last.BytesWritten != *last.ResponseBytes {
			t.Fatal("failed delivery was reported as success", last)
		}
		raw, _ := json.Marshal(events)
		for _, secret := range []string{token, "alice", "image.example", "synthetic-title", "synthetic private network message"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("cover diagnostics contain private data", secret)
			}
		}
	}
}
