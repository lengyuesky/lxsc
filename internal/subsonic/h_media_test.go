package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lxsc/internal/assets"
	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestCoverRequestsDoNotExpandBoardsOrOnlineAlbums(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, _ := database.CreateUser(ctx, "user", "enc", false, "320k")
	store, _ := settings.New(ctx, database)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	calls := 0
	catalog.SetRequestObserver(func(music.RemoteRequest) { calls++ })
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log, HTTP: &http.Client{}}

	oaID := music.OnlineAlbumID("wy", "album", "专辑", "歌手", "https://img.example/album.jpg", "sap-parent")
	for _, test := range []struct {
		id   string
		code int
	}{
		{oaID, http.StatusFound},
		{music.BoardID("wy", "hot"), http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, "/rest/getCoverArt.view?id="+test.id, nil)
		req = req.WithContext(withUser(req.Context(), user))
		recorder := httptest.NewRecorder()
		server.getCoverArt(recorder, req)
		if recorder.Code != test.code {
			t.Fatalf("封面状态异常 id=%s got=%d want=%d", test.id, recorder.Code, test.code)
		}
	}
	if calls != 0 {
		t.Fatalf("封面请求不应扫描榜单或专辑歌曲: %d", calls)
	}
}

func TestDownloadDoesNotPersistButStreamDoes(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.CreateUser(ctx, "user", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &http.Client{}
	sources := js.NewSourceManager(string(prelude), client, client, log)
	defer sources.UnloadAll()
	script := `
const { EVENT_NAMES, on, send } = globalThis.lx
on(EVENT_NAMES.request, ({ source, action, info }) => {
  if (action === 'musicUrl') return Promise.resolve('https://cdn.example/' + source + '/' + info.musicInfo.songmid + '.mp3')
  return Promise.reject(new Error('unsupported'))
})
send(EVENT_NAMES.inited, { status: true, sources: { wy: { name: '测试', type: 'music', actions: ['musicUrl'], qualitys: ['320k'] } } })`
	if _, err := sources.Load(ctx, 1, 1, script); err != nil {
		t.Fatal(err)
	}
	catalog := music.NewCatalog(database, nil, sources, store, log)
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "temporary", "name": "临时歌曲", "singer": "歌手", "albumName": "专辑", "types": []any{map[string]any{"type": "320k"}}})
	catalog.Cache([]*music.Info{info})
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log, HTTP: client}
	server.HTTP = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 206, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	})}

	request := func(handler func(http.ResponseWriter, *http.Request)) {
		req := httptest.NewRequest(http.MethodGet, "/rest/media.view?id="+info.TrackID()+"&f=json", nil)
		req = req.WithContext(withUser(req.Context(), user))
		recorder := httptest.NewRecorder()
		handler(recorder, req)
		if recorder.Code != http.StatusFound {
			t.Fatalf("媒体接口应返回重定向，实际 %d: %s", recorder.Code, recorder.Body.String())
		}
	}

	request(server.download)
	if _, err := database.GetTrack(ctx, info.TrackID()); err == nil {
		t.Fatal("download 不应持久化临时歌曲")
	}
	request(server.stream)
	if _, err := database.GetTrack(ctx, info.TrackID()); err != nil {
		t.Fatalf("stream 成功后应持久化歌曲: %v", err)
	}

	if _, err := store.Update(ctx, map[string]json.RawMessage{"streamMode": json.RawMessage(`"proxy"`)}); err != nil {
		t.Fatal(err)
	}
	failures := []struct {
		name      string
		id        string
		transport roundTripFunc
	}{
		{"网络错误", "network", func(*http.Request) (*http.Response, error) { return nil, errors.New("network down") }},
		{"HTTP 500", "status-500", func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Header: make(http.Header), Body: io.NopCloser(strings.NewReader("failed")), Request: request}, nil
		}},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			failedInfo := music.FromMap(map[string]any{"source": "wy", "songmid": "failed-" + failure.id, "name": "失败歌曲", "singer": "歌手", "albumName": "专辑", "types": []any{map[string]any{"type": "320k"}}})
			catalog.Cache([]*music.Info{failedInfo})
			server.HTTP = &http.Client{Transport: failure.transport}
			req := httptest.NewRequest(http.MethodGet, "/rest/stream.view?id="+failedInfo.TrackID()+"&f=json", nil)
			req = req.WithContext(withUser(req.Context(), user))
			recorder := httptest.NewRecorder()
			server.stream(recorder, req)
			if _, err := database.GetTrack(ctx, failedInfo.TrackID()); err == nil {
				t.Fatal("代理失败不应持久化歌曲")
			}
		})
	}
}
