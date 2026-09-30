package subsonic

import (
	"context"
	"encoding/json"
	"lxsc/internal/music"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaybackEndpointsPersistAndSerialize(t *testing.T) {
	f := newDirectoryTestServer(t)
	track := music.FromMap(map[string]any{"source": "wy", "songmid": "1", "name": "test"})
	if err := f.server.Catalog.RememberSync(context.Background(), []*music.Info{track}); err != nil {
		t.Fatal(err)
	}
	call := func(handler http.HandlerFunc, query string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "/rest/test?f=json&"+query, nil)
		r = r.WithContext(withUser(r.Context(), f.user))
		w := httptest.NewRecorder()
		handler(w, r)
		var out map[string]map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out["subsonic-response"]
	}
	if root := call(f.server.savePlayQueue, "id=tr-wy-1&id=tr-wy-1&current=tr-wy-1&position=1250&c=phone"); root["status"] != "ok" {
		t.Fatal(root)
	}
	root := call(f.server.getPlayQueue, "")
	queue := root["playQueue"].(map[string]any)
	if len(queue["entry"].([]any)) != 2 || queue["position"] != float64(1250) {
		t.Fatal(queue)
	}
	if root = call(f.server.savePlayQueue, "id=tr-wy-1&current=tr-wy-2"); root["status"] != "failed" {
		t.Fatal("invalid current accepted")
	}
	if root = call(f.server.setRating, "id=tr-wy-1&rating=5"); root["status"] != "ok" {
		t.Fatal(root)
	}
	root = call(f.server.getSong, "id=tr-wy-1")
	if root["song"].(map[string]any)["userRating"] != float64(5) {
		t.Fatal(root)
	}
	if root = call(f.server.scrobble, "id=tr-wy-1&submission=false&c=phone"); root["status"] != "ok" {
		t.Fatal(root)
	}
	root = call(f.server.getNowPlaying, "")
	if len(root["nowPlaying"].(map[string]any)["entry"].([]any)) != 1 {
		t.Fatal(root)
	}
	r := httptest.NewRequest("GET", "/rest/getPlayQueue?f=xml", nil)
	r = r.WithContext(withUser(r.Context(), f.user))
	w := httptest.NewRecorder()
	f.server.getPlayQueue(w, r)
	if !strings.Contains(w.Body.String(), `<playQueue`) || !strings.Contains(w.Body.String(), `position="1250"`) {
		t.Fatal(w.Body.String())
	}
}
func TestStarWriteFailureIsNotReportedAsSuccess(t *testing.T) {
	f := newDirectoryTestServer(t)
	f.database.Close()
	for _, handler := range []http.HandlerFunc{f.server.unstar, f.server.getStarred} {
		r := httptest.NewRequest("GET", "/rest/test?f=json&id=tr-wy-1", nil)
		r = r.WithContext(withUser(r.Context(), f.user))
		w := httptest.NewRecorder()
		handler(w, r)
		if !strings.Contains(w.Body.String(), `"status":"failed"`) {
			t.Fatal(w.Body.String())
		}
	}
}
