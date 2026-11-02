package fnmusic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

const searchTestSDK = `globalThis.__sdk_call = (path,args) => {
  const source=path.split('.')[0], query=args[0];
  if(query === 'empty' || query === 'public' || query === 'pl-bob') return {list:[]};
  if(path.endsWith('.getLyric')) return {lyric:'[00:01.00]search lyric'};
  if(path.endsWith('.searchSinger')) return {list:[{id:source+'-singer',name:query+' artist '+source,albumSize:1,picUrl:'https://image.example/artist'}]};
  if(path.endsWith('.searchAlbum') || path.endsWith('.getArtistAlbums')) return {list:[{id:source+'-album',name:query+' album '+source,artistName:query+' artist '+source,artistId:source+'-singer',size:2,picUrl:'https://image.example/album'}],total:1};
  if(path.endsWith('.getAlbumSongs')) return {list:[{source,songmid:'album-child',name:'child track',singer:'child artist',albumId:query,albumName:'child album',img:'https://image.example/child'}]};
  const artist=query.endsWith(' artist '+source)?query:query+' artist '+source;
  if(path.endsWith('.musicSearch.search')) return {list:[
    {source,songmid:'remote-one',name:query+' one '+source,singer:artist,albumId:source+'-album',albumName:query+' album '+source,img:'https://image.example/one'},
    {source,songmid:'remote-two',name:query+' two '+source,singer:artist,albumId:source+'-album',albumName:query+' album '+source,img:'https://image.example/two'}
  ]};
  return {list:[]};
};`

func searchFixture(t *testing.T, sources string) *fixture {
	t.Helper()
	f := newFixture(t)
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"searchSources": json.RawMessage(sources)}); err != nil {
		t.Fatal(err)
	}
	lyricSDK(t, f, searchTestSDK)
	return f
}

func searchRequest(t *testing.T, f *fixture, token, kind, query, extra string) map[string]any {
	t.Helper()
	return payload(t, request(f, http.MethodGet, "/music/api/v1/search/"+kind+"?q="+url.QueryEscape(query)+extra, token, nil))
}

func TestSearchFourEndpointsAndSuggestionsHaveClientShapes(t *testing.T) {
	f := searchFixture(t, `["wy","tx"]`)
	token := login(t, f, "alice")
	for _, kind := range []string{"track", "album", "artist"} {
		data := searchRequest(t, f, token, kind, "remote", "")
		items := data["list"].([]any)
		want := 2
		if kind == "track" {
			want = 4
		}
		if len(items) != want || data["total"] != float64(want) {
			t.Fatal("search did not return a bounded aggregate result", kind, len(items), data["total"])
		}
		for _, value := range items {
			view := value.(map[string]any)
			if !validGUID(view["guid"].(string)) || !strings.HasPrefix(view["coverId"].(string), kind+"_") {
				t.Fatal("search result lacks a navigable resource identifier", kind)
			}
		}
	}
	playlist := searchRequest(t, f, token, "playlist", "pl-", "")
	if playlist["total"] != float64(2) {
		t.Fatal("playlist search included another user's private playlist")
	}
	suggestions := searchRequest(t, f, token, "suggest", "remote", "")
	for _, kind := range []string{"track", "album", "artist", "playlist"} {
		group := suggestions[kind].(map[string]any)
		if _, ok := group["items"].([]any); !ok {
			t.Fatal("suggestion groups must contain arrays, including empty groups", kind)
		}
		if group["total"] == nil {
			t.Fatal("suggestion totals missing", kind)
		}
	}
}

func TestSearchPaginationIsStableAndOnlyReturnedTracksAreGranted(t *testing.T) {
	f := searchFixture(t, `["wy","tx"]`)
	token := login(t, f, "alice")
	first := searchRequest(t, f, token, "track", "remote", "&size=1&page=1")
	second := searchRequest(t, f, token, "track", "remote", "&size=1&page=2")
	firstView, secondView := first["list"].([]any)[0].(map[string]any), second["list"].([]any)[0].(map[string]any)
	if first["total"] != float64(4) || second["total"] != float64(4) || firstView["guid"] == secondView["guid"] || firstView["title"] != "remote one wy" || secondView["title"] != "remote one tx" {
		t.Fatal("global pagination lost configured platform order or skipped a result")
	}
	guid := identifier("track", "tr-wy-remote-two")
	if request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, token, nil).Code != http.StatusNotFound {
		t.Fatal("off-page global cache entry became a session permission")
	}
	last := searchRequest(t, f, token, "track", "remote", "&size=1&page=4")
	if len(last["list"].([]any)) != 1 {
		t.Fatal("last known result page is missing")
	}
	end := searchRequest(t, f, token, "track", "remote", "&size=1&page=2147483647")
	if len(end["list"].([]any)) != 0 || end["total"] != float64(4) {
		t.Fatal("out-of-range page fabricated a platform total or overflowed")
	}
}

func TestSearchResultSupportsCoverLyricsMetadataAndRealDelivery(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	_, body := attachMedia(t, f)
	token := login(t, f, "alice")
	data := searchRequest(t, f, token, "track", "remote", "")
	view := data["list"].([]any)[0].(map[string]any)
	guid := view["guid"].(string)
	picture := coverPNG(t)
	f.server.Covers.HTTP = &http.Client{Transport: resourceTransport(func(*http.Request) (*http.Response, error) {
		return imageResponse(http.StatusOK, picture), nil
	})}
	w := request(f, http.MethodGet, "/music/api/v1/static/cover?coverId="+view["coverId"].(string), token, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), picture) {
		t.Fatal("searched track cover was not accessible to its session")
	}
	lyrics := lyricPayload(t, request(f, http.MethodGet, "/music/api/v1/lyric/list?trackGUID="+guid, token, nil))
	if len(lyrics.List) != 1 {
		t.Fatal("searched track cannot load lyrics")
	}
	meta := payload(t, request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, token, nil))
	if meta["track"].(map[string]any)["guid"] != guid {
		t.Fatal("searched track changed identity during metadata lookup")
	}
	if _, err := f.db.GetTrack(context.Background(), "tr-wy-remote-one"); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("search or metadata lookup persisted unsolicited track data", err)
	}
	w = request(f, http.MethodGet, "/music/api/v1/track/stream?guid="+guid, token, nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") == "" {
		t.Fatal("searched result cannot use the shared direct-link player")
	}
	if _, err := f.server.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)}); err != nil {
		t.Fatal(err)
	}
	w = request(f, http.MethodGet, "/music/api/v1/track/stream?guid="+guid, token, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatal("searched result did not deliver actual audio in the configured mode")
	}
	if _, err := f.db.GetTrack(context.Background(), "tr-wy-remote-one"); err != nil {
		t.Fatal("explicit playback no longer follows the shared metadata persistence policy", err)
	}
	if _, _, err := f.db.ResolveClientID(context.Background(), "fnmusic", guid); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("transient search created durable protocol identifiers", err)
	}
	library := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	if library["total"] != float64(2) {
		t.Fatal("search changed favorites, playlists or the user's library")
	}
}

func TestSearchGrantIsIsolatedFromOtherUsersAndOtherSessions(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	alice, aliceOther, bob := login(t, f, "alice"), login(t, f, "alice"), login(t, f, "bob")
	result := searchRequest(t, f, alice, "track", "remote", "")
	view := result["list"].([]any)[0].(map[string]any)
	guid := view["guid"].(string)
	for _, token := range []string{aliceOther, bob} {
		for _, path := range []string{
			"/music/api/v1/track/metadata?guid=" + guid,
			"/music/api/v1/track/stream?guid=" + guid,
			"/music/api/v1/lyric/list?trackGUID=" + guid,
			"/music/api/v1/static/cover?coverId=" + view["coverId"].(string),
		} {
			if w := request(f, http.MethodGet, path, token, nil); w.Code != http.StatusNotFound {
				t.Fatal("another session gained access through shared search cache", w.Code)
			}
		}
	}
	_ = searchRequest(t, f, bob, "track", "remote", "")
	if request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, bob, nil).Code != http.StatusOK {
		t.Fatal("independent search did not grant its caller access to the cached public result")
	}
	_ = payload(t, request(f, http.MethodPost, "/music/api/v1/user/logout", alice, nil))
	if request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+guid, alice, nil).Code != http.StatusUnauthorized {
		t.Fatal("logout retained usable search permissions")
	}
	ctx := context.WithValue(context.Background(), sessionKey{}, sha256.Sum256([]byte(alice)))
	if _, ok := f.server.searches.get(ctx, "track", guid, time.Now()); ok {
		t.Fatal("logout did not remove session-scoped grants")
	}
}

func TestSearchDoesNotGrantAccessToRevokedLibraryResults(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	token := login(t, f, "alice")
	result := searchRequest(t, f, token, "track", "public", "")
	view := result["list"].([]any)[0].(map[string]any)
	no := false
	if err := f.db.UpdatePlaylistMeta(context.Background(), "pl-public", nil, nil, &no); err != nil {
		t.Fatal(err)
	}
	guid := view["guid"].(string)
	for _, path := range []string{"/music/api/v1/track/metadata?guid=" + guid, "/music/api/v1/static/cover?coverId=" + view["coverId"].(string), "/music/api/v1/lyric/list?trackGUID=" + guid} {
		if request(f, http.MethodGet, path, token, nil).Code != http.StatusNotFound {
			t.Fatal("local library filtering created a new grant that bypassed revocation")
		}
	}
	playlist := searchRequest(t, f, token, "playlist", "pl-bob", "")
	if len(playlist["list"].([]any)) != 0 {
		t.Fatal("private playlist leaked into search")
	}
}

func TestSearchAlbumAndArtistCanOpenWithoutPersistingResources(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	token := login(t, f, "alice")
	other := login(t, f, "bob")
	for _, kind := range []string{"album", "artist"} {
		result := searchRequest(t, f, token, kind, "remote", "")
		view := result["list"].([]any)[0].(map[string]any)
		guid := view["guid"].(string)
		path := "/music/api/v1/" + kind + "/detail?guid=" + guid
		if data := payload(t, request(f, http.MethodGet, path, token, nil)); data["guid"] != guid {
			t.Fatal("searched entity cannot open its detail page", kind)
		}
		if request(f, http.MethodGet, path, other, nil).Code != http.StatusNotFound {
			t.Fatal("entity detail treated a global summary cache as permission", kind)
		}
		tracks := payload(t, request(f, http.MethodGet, "/music/api/v1/track/"+kind+"-detail/list?"+kind+"GUID="+guid, token, nil))
		if len(tracks["list"].([]any)) == 0 {
			t.Fatal("searched entity has no navigable child tracks", kind)
		}
		child := tracks["list"].([]any)[0].(map[string]any)
		if request(f, http.MethodGet, "/music/api/v1/track/metadata?guid="+child["guid"].(string), token, nil).Code != http.StatusOK {
			t.Fatal("entity child track was not granted to the caller")
		}
		if kind == "artist" {
			albums := payload(t, request(f, http.MethodGet, "/music/api/v1/album/artist-detail/list?artistGUID="+guid, token, nil))
			if len(albums["list"].([]any)) != 1 {
				t.Fatal("artist albums did not use the existing core")
			}
		}
		if _, _, err := f.db.ResolveClientID(context.Background(), "fnmusic", guid); !errors.Is(err, db.ErrNotFound) {
			t.Fatal("online entity search persisted a protocol mapping")
		}
	}
}

func TestSearchEmptyFailureValidationAndPrivacy(t *testing.T) {
	f := searchFixture(t, `["wy"]`)
	token := login(t, f, "alice")
	for _, kind := range []string{"track", "album", "artist", "playlist"} {
		for _, query := range []string{"", "empty"} {
			data := searchRequest(t, f, token, kind, query, "")
			if data["list"] == nil || len(data["list"].([]any)) != 0 || data["total"] != float64(0) {
				t.Fatal("empty search result is not an empty array", kind)
			}
		}
		if request(f, http.MethodGet, "/music/api/v1/search/"+kind+"?q=remote", "", nil).Code != http.StatusUnauthorized {
			t.Fatal("search does not require authentication")
		}
	}
	for _, query := range []string{strings.Repeat("a", 513), "unsafe\x00query", "unsafe\nquery"} {
		if request(f, http.MethodGet, "/music/api/v1/search/track?q="+url.QueryEscape(query), token, nil).Code != http.StatusBadRequest {
			t.Fatal("invalid query was not rejected")
		}
	}
	lyricSDK(t, f, `globalThis.__sdk_call=()=>{throw new Error('private upstream token and query text')};`)
	for _, kind := range []string{"track", "album", "artist"} {
		w := request(f, http.MethodGet, "/music/api/v1/search/"+kind+"?q=secret-search-query", token, nil)
		if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "private upstream") {
			t.Fatal("all-source failure was masked as an empty search or leaked an error", kind, w.Code)
		}
	}
	raw, _ := json.Marshal(f.server.Diagnostics.List())
	for _, sensitive := range []string{token, "alice", "secret-search-query", "private upstream token", "image.example"} {
		if bytes.Contains(raw, []byte(sensitive)) {
			t.Fatal("search diagnostics log text or credentials", sensitive)
		}
	}
}

func TestSearchGrantExpiryCapacityAndNoAccessByIdentifierAlone(t *testing.T) {
	store := searchStore{}
	session := sha256.Sum256([]byte("synthetic-session"))
	ctx := context.WithValue(context.Background(), sessionKey{}, session)
	now := time.Now()
	track := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "synthetic"})
	first := identifier("track", "tr-wy-first")
	store.grant(ctx, "track", first, searchResource{track: track}, now)
	for i := range searchSessionGrantLimit {
		store.grant(ctx, "track", identifier("track", fmt.Sprint("tr-wy-", i)), searchResource{track: track}, now)
	}
	if store.entries.Len() != searchSessionGrantLimit || store.counts[session] != searchSessionGrantLimit {
		t.Fatal("session grants are not bounded")
	}
	if _, ok := store.get(ctx, "track", first, now); ok {
		t.Fatal("oldest grant was not evicted")
	}
	last := identifier("track", fmt.Sprint("tr-wy-", searchSessionGrantLimit-1))
	if _, ok := store.get(context.Background(), "track", last, now); ok {
		t.Fatal("GUID alone granted access without a validated session")
	}
	if _, ok := store.get(ctx, "track", last, now.Add(searchGrantTTL)); ok {
		t.Fatal("expired grant was kept alive by cache access")
	}
	store.revoke(session)
	if store.entries.Len() != 0 || len(store.counts) != 0 {
		t.Fatal("revoked grants or session accounting leaked")
	}
}

func TestSearchGrantGlobalCapacityAndAccounting(t *testing.T) {
	store := searchStore{}
	now := time.Now()
	track := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "synthetic"})
	for sessionIndex := range 5 {
		session := sha256.Sum256([]byte(fmt.Sprint("synthetic-session-", sessionIndex)))
		ctx := context.WithValue(context.Background(), sessionKey{}, session)
		for index := range searchSessionGrantLimit {
			store.grant(ctx, "track", identifier("track", fmt.Sprint("tr-wy-", index)), searchResource{track: track}, now)
		}
	}
	if store.entries.Len() != searchGrantLimit {
		t.Fatal("global search grants are unbounded", store.entries.Len())
	}
	count := 0
	for _, size := range store.counts {
		if size > searchSessionGrantLimit || size < 1 {
			t.Fatal("per-session eviction accounting is inconsistent", size)
		}
		count += size
	}
	if count != store.entries.Len() {
		t.Fatal("evicted grants remained in session counters")
	}
}

func TestSearchCancellationReleasesSharedWork(t *testing.T) {
	f := searchFixture(t, `["wy","tx"]`)
	token := login(t, f, "alice")
	lyricSDK(t, f, `globalThis.__sdk_call=()=>new Promise(()=>{});`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/music/api/v1/search/track?q=cancelled-search", nil).WithContext(ctx)
	r.AddCookie(&http.Cookie{Name: "music-token", Value: token})
	w := httptest.NewRecorder()
	started := time.Now()
	f.routes.ServeHTTP(w, r)
	if time.Since(started) > 2*time.Second {
		t.Fatal("cancelled search waited for the full upstream budget")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := f.server.Catalog.Performance()
		search := stats["searchCache"].(music.CacheStats)
		if f.server.Catalog.RequestLimits.Stats().Active == 0 && search.InFlight == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cancelled search leaked request or shared-work permits")
}
