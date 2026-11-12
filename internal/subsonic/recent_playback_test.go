package subsonic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/js"
)

func TestCrossPlatformPlaybackLogRecordsWinnerAndFeedback(t *testing.T) {
	for _, mode := range []string{"redirect", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			script := `// @name 跨平台测试音源
	lx.on(lx.EVENT_NAMES.request, ({source}) => Promise.resolve('https://media.invalid/'+source));
	lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']},kg:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`
			s, user, info := newMediaStabilityServer(t, script)
			prelude, err := assets.JS.ReadFile("js/prelude.js")
			if err != nil {
				t.Fatal(err)
			}
			sdk := `globalThis.__sdk_call=(path,args)=>Promise.resolve({list:path.startsWith('kg.') ? [{source:'kg',songmid:'alternate',name:'临时歌曲',singer:'测试歌手',interval:'00:45',types:[{type:'320k'}]}] : []});`
			pool, err := js.NewSDKPool(1, string(prelude), sdk, &http.Client{}, &http.Client{}, s.Log)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			s.Catalog.SDK = pool
			s.Diagnostics = &diagnostics.Events{}
			setSourceConcurrency(t, s, 1)
			raw, _ := json.Marshal(mode)
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
				t.Fatal(err)
			}
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status := http.StatusInternalServerError
				if r.URL.Path == "/kg" {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"audio/wav"}}, Body: io.NopCloser(strings.NewReader("合成音频")), Request: r}, nil
			})}
			w := httptest.NewRecorder()
			s.stream(w, mediaStabilityRequest(user, info, "&c=合成客户端"))
			if mode == "redirect" && w.Code != http.StatusFound {
				t.Fatalf("跨平台兜底302失败%d", w.Code)
			}
			if mode == "proxy" && (w.Code != http.StatusOK || w.Body.String() != "合成音频") {
				t.Fatalf("跨平台兜底代理失败%d：%s", w.Code, w.Body.String())
			}
			page, err := s.DB.RecentPlayback(context.Background(), db.PlaybackQuery{UserID: user.ID}, time.Now())
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("日志缺失：%+v %v", page, err)
			}
			log := page.Items[0]
			if log.RequestedPlatform != "wy" || log.Platform != "kg" || log.ResolvedTrackID != "tr-kg-alternate" || log.SourceID != 1 || log.Outcome != "success" || log.Client != "合成客户端" || log.Kind != "request" {
				t.Fatalf("必须保留原曲平台并记录真实获胜平台及曲目：%+v", log)
			}
			found := false
			for _, state := range s.Catalog.Sources.ScheduleSnapshot(1) {
				if state.Platform == "kg" && state.Action == "media" && state.Success == 1 {
					found = true
				}
			}
			if !found {
				t.Fatal("跨平台胜出必须向真实平台反馈媒体成功评分")
			}
			detail, err := s.DB.PlaybackDetail(context.Background(), log.ID, user.ID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			cross, failed := false, false
			for _, event := range detail.Events {
				cross = cross || (event.Stage == "cross_source" && event.Platform == "kg")
				failed = failed || (event.Platform == "wy" && event.Status == 500)
			}
			if !cross || !failed {
				t.Fatalf("详情必须同时包含原平台失败及最终兜底：%+v", detail.Events)
			}
		})
	}
}

func TestSubsonicPlaybackProtocolFailureKeepsActualHTTPStatus(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("拒绝")), Request: r}, nil
	})}
	w := httptest.NewRecorder()
	s.stream(w, mediaStabilityRequest(user, info, "&c=客户端"))
	page, err := s.DB.RecentPlayback(context.Background(), db.PlaybackQuery{UserID: user.ID}, time.Now())
	if err != nil || len(page.Items) != 1 || page.Items[0].Outcome != "failed" || page.Items[0].Status != w.Code || page.Items[0].Delivery != "" {
		t.Fatalf("Subsonic的HTTP 200协议失败不能当交付成功：HTTP %d %+v %v", w.Code, page, err)
	}
}
