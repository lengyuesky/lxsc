package mediastream

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

type interruptedAudio struct{ sent bool }

func (r *interruptedAudio) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "synthetic"), nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (r *interruptedAudio) Close() error { return nil }

func TestProxyCopyFailureAndCancellationRemainFailedInClientEvent(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := settings.New(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := &Service{Settings: store, Log: log, Diagnostics: &diagnostics.Events{}}
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "test"})
	resolution := music.URLResolution{Result: js.MusicURLResult{Quality: "320k"}}
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if canceled {
			cancel()
		}
		event := &diagnostics.Event{Result: "failed", Error: "none"}
		r := Bind(httptest.NewRequest(http.MethodGet, "/media", nil).WithContext(ctx), &db.User{ID: 1}, event)
		body := io.ReadCloser(&interruptedAudio{})
		if canceled {
			body = io.NopCloser(strings.NewReader("not delivered"))
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"audio/wav"}}, Body: body}
		ok := service.ProxyStream(httptest.NewRecorder(), r, info, resolution, response)
		cancel()
		if ok || event.Result == "ok" {
			t.Fatal("失败或取消的正文被记为客户端成功")
		}
		if !canceled && (event.Status != http.StatusOK || event.Error == "none") {
			t.Fatal("提交2xx之后的正文错误没有保留失败结果")
		}
	}
}
