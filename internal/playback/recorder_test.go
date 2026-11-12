package playback

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/db"
)

type recorderFixture struct {
	r        *Recorder
	d        *db.DB
	u, other *db.User
	clock    atomic.Int64
}

func recorderTestFixture(t *testing.T) *recorderFixture {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	f := &recorderFixture{d: d}
	f.u, err = d.CreateUser(context.Background(), "alice", "fake", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	f.other, err = d.CreateUser(context.Background(), "bob", "fake", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Store(time.Now().UnixMilli())
	f.r = New(d, slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.r.now = func() time.Time { return time.UnixMilli(f.clock.Load()) }
	return f
}

func (f *recorderFixture) now() time.Time          { return time.UnixMilli(f.clock.Load()) }
func (f *recorderFixture) advance(d time.Duration) { f.clock.Add(d.Milliseconds()) }
func (f *recorderFixture) stats(t *testing.T, user int64) db.ListeningStats {
	t.Helper()
	s, err := f.d.ListeningStatistics(context.Background(), user, 30, f.now())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFnStartUsesFixedWindowAndSeparatesUsersAndSessions(t *testing.T) {
	f := recorderTestFixture(t)
	ctx := context.Background()
	track := db.PlaybackTrack{ID: "tr-wy-1", Name: "歌曲", Singer: "歌手", Platform: "wy"}
	start := func(user *db.User, session string) {
		t.Helper()
		if err := f.r.FnStart(ctx, user, session, track, 180000); err != nil {
			t.Fatal(err)
		}
	}
	start(f.u, "independent-session-a")
	f.advance(4 * time.Second)
	start(f.u, "independent-session-a")
	if stats := f.stats(t, f.u.ID); stats.Plays != 1 || stats.ClientMS != 180000 {
		t.Fatalf("五秒内重复开始不能累计：%+v", stats)
	}
	f.advance(time.Second)
	start(f.u, "independent-session-a")
	if stats := f.stats(t, f.u.ID); stats.Plays != 2 {
		t.Fatal("重复事件不能滑动延长固定窗口")
	}
	start(f.u, "independent-session-b")
	start(f.other, "independent-session-a")
	if f.stats(t, f.u.ID).Plays != 3 || f.stats(t, f.other.ID).Plays != 1 {
		t.Fatal("不同用户或登录会话必须分别计数")
	}
}

func TestFnSwitchingSongsBreaksDuplicateWindow(t *testing.T) {
	f := recorderTestFixture(t)
	for _, id := range []string{"tr-wy-1", "tr-wy-2", "tr-wy-1"} {
		if err := f.r.FnStart(context.Background(), f.u, "one-session", db.PlaybackTrack{ID: id, Name: id, Platform: "wy"}, 0); err != nil {
			t.Fatal(err)
		}
		f.advance(time.Second)
	}
	if stats := f.stats(t, f.u.ID); stats.Plays != 3 || stats.UnknownDuration != 3 || stats.ClientMS != 0 {
		t.Fatalf("切到别的歌再回来应计为新播放，缺时长仅计次：%+v", stats)
	}
}

func TestFnMediaThenStartAndRangeShareLogWithoutExtraListening(t *testing.T) {
	f := recorderTestFixture(t)
	track := db.PlaybackTrack{ID: "tr-wy-1", Name: "歌曲", Singer: "歌手", Platform: "wy"}
	request := func() *http.Request {
		return Bind(httptest.NewRequest("GET", "/music/api/v1/track/stream", nil), Client{Protocol: "fnmusic", Name: "飞牛音乐", Session: "independent-session"})
	}
	_, trace := f.r.Begin(request(), f.u, track.ID, "flac", "proxy")
	trace.Identify(track)
	trace.Add(db.PlaybackEvent{Stage: "resolve", SourceID: 3, SourceName: "测试音源", Platform: "wy", Quality: "flac"})
	trace.Selected("wy", track.ID, 3, "测试音源", "flac", "", false)
	trace.Delivery("proxy")
	trace.Outcome("success", 200)
	trace.Finish()
	page, err := f.d.RecentPlayback(context.Background(), db.PlaybackQuery{UserID: f.u.ID}, f.now())
	if err != nil || len(page.Items) != 1 || page.Items[0].Kind != "request" || f.stats(t, f.u.ID).Plays != 0 {
		t.Fatalf("媒体请求不能直接计为收听：%+v %v", page, err)
	}
	id := page.Items[0].ID
	f.advance(time.Second)
	if err := f.r.FnStart(context.Background(), f.u, "independent-session", track, 180000); err != nil {
		t.Fatal(err)
	}
	rangeRequest := request()
	rangeRequest.Header.Set("Range", "bytes=100-200")
	_, trace = f.r.Begin(rangeRequest, f.u, track.ID, "flac", "proxy")
	trace.Identify(track)
	trace.Selected("wy", track.ID, 3, "测试音源", "flac", "proxy", true)
	trace.Outcome("success", 206)
	trace.Finish()
	page, err = f.d.RecentPlayback(context.Background(), db.PlaybackQuery{UserID: f.u.ID}, f.now())
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != id || page.Items[0].Kind != "playback" || page.Items[0].Requests != 2 || page.Items[0].Status != 206 || f.stats(t, f.u.ID).Plays != 1 {
		t.Fatalf("开始事件关联原交付，Range不增加收听：%+v %v", page, err)
	}
}

func TestPlaybackHasLiveDetailsAndDoesNotStoreAuthentication(t *testing.T) {
	f := recorderTestFixture(t)
	const secret = "fake-authentication-value-must-not-be-recorded"
	r := httptest.NewRequest("GET", "/api/app/stream?apiKey="+secret, nil)
	r.Header.Set("Authorization", "Bearer "+secret)
	r.AddCookie(&http.Cookie{Name: "music-token", Value: secret})
	r = Bind(r, Client{Protocol: "web", Name: "网页播放器", Key: "independent-playback-id"})
	_, trace := f.r.Begin(r, f.u, "tr-wy-1", "flac", "proxy")
	trace.Identify(db.PlaybackTrack{ID: "tr-wy-1", Name: "歌曲", Platform: "wy"})
	trace.Add(db.PlaybackEvent{Stage: "resolve", SourceID: 1, SourceName: "音源", Platform: "wy", Error: "none"})
	trace.Selected("wy", "tr-wy-1", 1, "音源", "flac", "", false)
	trace.Response(200)
	trace.Delivery("proxy")
	page, err := f.d.RecentPlayback(context.Background(), db.PlaybackQuery{UserID: f.u.ID}, f.now())
	if err != nil || len(page.Items) != 1 || page.Items[0].Outcome != "pending" || page.Items[0].SourceName != "音源" || page.Items[0].Delivery != "proxy" {
		t.Fatalf("传输结束前应能查看源和交付方式：%+v %v", page, err)
	}
	trace.Add(db.PlaybackEvent{Stage: "proxy_copy", Error: "cancelled"})
	trace.Outcome("interrupted", 200)
	trace.Finish()
	trace.Finish()
	detail, err := f.d.PlaybackDetail(context.Background(), page.Items[0].ID, f.u.ID, f.now())
	if err != nil || detail.Outcome != "interrupted" || len(detail.Events) != 2 || detail.DetailCount != 2 {
		t.Fatalf("最终中断和明细应只保存一次：%+v %v", detail, err)
	}
	raw, _ := json.Marshal(detail)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "independent-playback-id") {
		t.Fatal("日志不得包含认证值或播放关联标识")
	}
}

func TestFnSameSongMediaBeforeRepeatDoesNotAttachToOldPlayback(t *testing.T) {
	f := recorderTestFixture(t)
	track := db.PlaybackTrack{ID: "tr-wy-1", Name: "歌曲", Platform: "wy"}
	ctx := context.Background()
	if err := f.r.FnStart(ctx, f.u, "same-session", track, 45000); err != nil {
		t.Fatal(err)
	}
	f.advance(6 * time.Second)
	r := Bind(httptest.NewRequest("GET", "/music/api/v1/track/stream", nil), Client{Protocol: "fnmusic", Name: "飞牛音乐", Session: "same-session"})
	_, trace := f.r.Begin(r, f.u, track.ID, "320k", "redirect")
	trace.Identify(track)
	trace.Selected("wy", track.ID, 1, "音源", "320k", "302", false)
	trace.Outcome("success", 302)
	trace.Finish()
	if err := f.r.FnStart(ctx, f.u, "same-session", track, 45000); err != nil {
		t.Fatal(err)
	}
	page, err := f.d.RecentPlayback(ctx, db.PlaybackQuery{UserID: f.u.ID}, f.now())
	if err != nil || len(page.Items) != 2 || page.Items[0].Kind != "playback" || page.Items[0].Delivery != "302" || page.Items[0].Requests != 1 || page.Items[1].Requests != 0 || f.stats(t, f.u.ID).Plays != 2 {
		t.Fatalf("同曲重播先取流后上报必须关联到新播放：%+v %v", page, err)
	}
}
