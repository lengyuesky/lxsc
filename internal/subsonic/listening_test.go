package subsonic

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

func TestScrobbleListeningOnlyCountsSubmissions(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	user, err := database.CreateUser(ctx, "测试用户", "enc", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog := music.NewCatalog(database, nil, nil, store, log)
	catalog.Cache([]*music.Info{music.FromMap(map[string]any{"source": "wy", "songmid": "1", "name": "歌曲", "singer": "歌手", "interval": "03:00"}), music.FromMap(map[string]any{"source": "wy", "songmid": "2", "name": "缺失时长"})})
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log}
	submit := func(query string) {
		t.Helper()
		request := httptest.NewRequest("GET", "/rest/scrobble?f=json&c=client&"+query, nil)
		request = request.WithContext(withUser(request.Context(), user))
		recorder := httptest.NewRecorder()
		server.scrobble(recorder, request)
		var result map[string]map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result["subsonic-response"]["status"] != "ok" {
			t.Fatalf("上报失败：%s", recorder.Body.String())
		}
	}
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	submit("id=tr-wy-1&time=" + stamp + "&submission=false")
	stats, err := database.ListeningStatistics(ctx, user.ID, 30, time.Now())
	if err != nil || stats.Plays != 0 {
		t.Fatal("正在播放通知不能计时")
	}
	for range 2 {
		submit("id=tr-wy-1&time=" + stamp)
	}
	submit("id=tr-wy-1&time=bad")
	submit("id=tr-wy-1&time=1")
	submit("id=tr-wy-1&time=" + strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10))
	for range 2 {
		submit("id=tr-wy-2")
	}
	stats, err = database.ListeningStatistics(ctx, user.ID, 30, time.Now())
	if err != nil || stats.ClientMS != 180000 || stats.Plays != 3 || stats.UnknownDuration != 2 {
		t.Fatalf("客户端去重或缺失时长口径错误：%+v，%v", stats, err)
	}
}
