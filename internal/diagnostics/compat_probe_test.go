package diagnostics

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"lxsc/internal/js"
)

func TestCompatibilityProbeScopesValidationAndDataIsolation(t *testing.T) {
	f := newFixture(t)
	_, read := f.create(t, false)
	view, probe := f.create(t, true)
	if w := f.call("GET", "/api/debug/capabilities", "", read, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "diagnosticsVersion") {
		t.Fatal(w.Code, w.Body)
	}
	for _, path := range []string{"/api/debug/probe/board", "/api/debug/probe/lyrics"} {
		for _, key := range []string{"", read} {
			w := f.call("POST", path, `{}`, key, nil)
			want := 403
			if key == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(path, w.Code)
			}
		}
		for _, body := range []string{`null`, `{"url":"https://PRIVATE"}`, `{"trackId":"tr-wy-1","trackId":"tr-wy-2"}`, `{"boardId":"pl-PRIVATE"}`, `{"boardId":"lb-wy-1?token=PRIVATE"}`, `{} {}`} {
			if w := f.call("POST", path, body, probe, nil); w.Code != 400 {
				t.Fatal(path, body, w.Code)
			}
		}
	}
	f.s.Catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		return json.RawMessage(`{"list":[{"source":"wy","songmid":"one","name":"PRIVATE歌曲","singer":"PRIVATE歌手"}]}`), nil
	})
	pool, err := js.NewSDKPool(1, "", `globalThis.__sdk_call=()=>({lyric:'[00:01.00]PRIVATE歌词'});`, &http.Client{}, &http.Client{}, f.s.Catalog.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	f.s.Catalog.SDK = pool
	before, _ := f.s.DB.Statistics(context.Background())
	for _, test := range []struct{ path, body, stage string }{
		{"board", `{"boardId":"lb-wy-one"}`, "board_probe"},
		{"lyrics", `{"trackId":"tr-wy-one"}`, "lyrics_probe"},
	} {
		w := f.call("POST", "/api/debug/probe/"+test.path, test.body, probe, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE") {
			t.Fatal(w.Code, w.Body)
		}
		var result struct {
			Events []Event
			Sample []string `json:"sampleTrackIds"`
		}
		if json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Events) != 1 || result.Events[0].Stage != test.stage || result.Events[0].Result != "ok" {
			t.Fatal(w.Body)
		}
		if test.path == "board" && (result.Events[0].Count == nil || *result.Events[0].Count != 1 || len(result.Sample) != 1) {
			t.Fatal(w.Body)
		}
		if test.path == "lyrics" && (result.Events[0].Lines == nil || *result.Events[0].Lines != 1 || result.Events[0].Synced == nil || !*result.Events[0].Synced) {
			t.Fatal(w.Body)
		}
	}
	after, _ := f.s.DB.Statistics(context.Background())
	if before.Tracks != after.Tracks || before.Albums != after.Albums {
		t.Fatal("探测不能持久化音乐资料")
	}
	f.s.Tokens.Revoke(view.ID)
	if w := f.call("POST", "/api/debug/probe/lyrics", `{"trackId":"tr-wy-one"}`, probe, nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestCompatibilityProbeCancellationAndSharedLimit(t *testing.T) {
	f := newFixture(t)
	view, key := f.create(t, true)
	for i := 0; i < cap(f.s.probes); i++ {
		f.s.probes <- struct{}{}
	}
	if w := f.call("POST", "/api/debug/probe/board", `{"boardId":"lb-wy-one"}`, key, nil); w.Code != 429 {
		t.Fatal(w.Code)
	}
	for i := 0; i < cap(f.s.probes); i++ {
		<-f.s.probes
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	f.s.Catalog.SetRemoteCallerForTest(func(ctx context.Context, _ string, _ ...any) (json.RawMessage, error) {
		close(started)
		select {
		case <-release:
			return json.RawMessage(`{"list":[]}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	done := make(chan int, 1)
	go func() { done <- f.call("POST", "/api/debug/probe/board", `{"boardId":"lb-wy-one"}`, key, nil).Code }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("探测未启动")
	}
	f.s.Tokens.Revoke(view.ID)
	select {
	case code := <-done:
		if code != 408 {
			t.Fatal(code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("撤销未中止探测")
	}
	if len(f.s.Events.List()) != 0 {
		t.Fatal("撤销后不能发布探测数据")
	}
}

func TestCompatibilityEventSanitization(t *testing.T) {
	events := &Events{}
	n := 1
	events.Add(Event{Stage: "client_response", Endpoint: "PRIVATE", Client: "PRIVATE", Format: "PRIVATE", BoardID: "lb-wy-1?PRIVATE", BoardIDs: []string{"pl-PRIVATE", "lb-wy-one", "https://PRIVATE"}, Result: "PRIVATE", Count: &n, Error: "PRIVATE"})
	n = 99
	list := events.List()
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "PRIVATE") || *list[0].Count != 1 {
		t.Fatal(string(raw))
	}
	if len(list[0].BoardIDs) != 1 || list[0].BoardIDs[0] != "lb-wy-one" {
		t.Fatal(string(raw))
	}
	// 探测只能接受平台歌曲，不能通过任意 URL 或私有歌单 ID 扩大访问。
	if ValidBoardID("pl-private") || ValidTrackID("https://example.com") {
		t.Fatal("ID校验错误")
	}
}
