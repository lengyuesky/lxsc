package fnmusic

import (
	"context"
	"net/http"
	"testing"
	"time"

	"lxsc/internal/db"
)

func TestFnPlaybackReportsEstimateAndAssociateMedia(t *testing.T) {
	f := newFixture(t)
	attachMedia(t, f)
	token := login(t, f, "alice")
	_ = payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
	guid := identifier("track", "tr-wy-one")
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		status := http.StatusFound
		if method == http.MethodHead {
			status = http.StatusOK
		}
		if w := request(f, method, "/music/api/v1/track/stream?guid="+guid, token, nil); w.Code != status {
			t.Fatalf("媒体请求失败%d", w.Code)
		}
	}
	stats := func() db.ListeningStats {
		t.Helper()
		value, err := f.db.ListeningStatistics(context.Background(), f.users["alice"].ID, 30, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if stats().Plays != 0 {
		t.Fatal("HEAD和GET本身不能增加收听统计")
	}
	report := func(id string) {
		t.Helper()
		body := map[string]any{"events": []any{map[string]any{"eventType": "track_play", "payload": map[string]string{"trackGUID": identifier("track", id)}}}}
		if w := request(f, http.MethodPost, "/music/api/v1/event/report", token, body); w.Code != 200 {
			t.Fatalf("开始事件失败%d：%s", w.Code, w.Body.String())
		}
	}
	report("tr-wy-one")
	report("tr-wy-one")
	if s := stats(); s.Plays != 1 || s.ClientMS != 45000 {
		t.Fatalf("开始上报应去重并按歌曲时长估算：%+v", s)
	}
	page, err := f.db.RecentPlayback(context.Background(), db.PlaybackQuery{UserID: f.users["alice"].ID}, time.Now())
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != "playback" || page.Items[0].Delivery != "302" || page.Items[0].SourceID != 1 {
		t.Fatalf("开始事件应关联已交付的媒体日志：%+v %v", page, err)
	}
	report("tr-wy-public")
	report("tr-wy-one")
	if s := stats(); s.Plays != 3 || s.ClientMS != 135000 {
		t.Fatalf("切到其他歌曲再回来，应计为新播放：%+v", s)
	}
}
