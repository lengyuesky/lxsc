package portal

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"lxsc/internal/db"
)

func TestRecentPlaybackAPIKeepsListDetailAndOptionsPrivate(t *testing.T) {
	f := newPortalFixture(t)
	ctx, at := context.Background(), time.Now()
	logs := map[string]int64{}
	for index, name := range []string{"alice", "bob"} {
		p := db.PlaybackStart{UserID: f.users[name].ID, UserName: name, Key: "playback-key", Protocol: "web", Client: name + "客户端", Kind: "playback", Track: db.PlaybackTrack{ID: "tr-wy-1", Name: name + "的歌曲", Platform: "wy"}, At: at.UnixMilli()}
		h, err := f.db.BeginPlayback(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		logs[name] = h.ID
		if err := f.db.CompletePlayback(ctx, h, db.PlaybackResult{Track: p.Track, Platform: "kg", SourceID: int64(index + 1), SourceName: name + "音源", Delivery: "302", Outcome: "success", Status: 302}, nil, at); err != nil {
			t.Fatal(err)
		}
	}
	alice, admin := f.client(t, "alice"), f.client(t, "admin")
	code, page := doJSON(t, alice, "GET", f.server.URL+"/listening/recent", nil)
	if code != 200 || len(page["items"].([]any)) != 1 || page["clients"].([]any)[0] != "alice客户端" || page["sources"].([]any)[0].(map[string]any)["name"] != "alice音源" {
		t.Fatalf("列表或筛选选项越权：%d %+v", code, page)
	}
	if code, _ := doJSON(t, alice, "GET", f.server.URL+"/listening/recent/"+strconv.FormatInt(logs["bob"], 10), nil); code != 404 {
		t.Fatalf("明细编号不能绕过归属，实际%d", code)
	}
	if code, page := doJSON(t, admin, "GET", f.server.URL+"/listening/recent?scope=all", nil); code != 200 || len(page["items"].([]any)) != 2 {
		t.Fatal("管理员应能查看全站")
	}
	if code, detail := doJSON(t, admin, "GET", f.server.URL+"/listening/recent/"+strconv.FormatInt(logs["bob"], 10)+"?scope=user&userId="+strconv.FormatInt(f.users["bob"].ID, 10), nil); code != 200 || detail["userName"] != "bob" {
		t.Fatalf("管理员指定用户明细错误：%+v", detail)
	}
	for _, query := range []string{"?scope=all", "?scope=user&userId=" + strconv.FormatInt(f.users["bob"].ID, 10)} {
		if code, _ := doJSON(t, alice, "GET", f.server.URL+"/listening/recent"+query, nil); code != 403 {
			t.Fatalf("普通用户不能请求%s", query)
		}
	}
	for _, query := range []string{"?scope=me&userId=2", "?cursor=bad", "?limit=101", "?sourceId=-1", "?delivery=invalid", "?outcome=invalid", "?days=31"} {
		if code, _ := doJSON(t, alice, "GET", f.server.URL+"/listening/recent"+query, nil); code != 400 {
			t.Fatalf("无效参数%s返回%d", query, code)
		}
	}
	if code, _ := doJSON(t, http.DefaultClient, "GET", f.server.URL+"/listening/recent", nil); code != 401 {
		t.Fatal("日志必须登录")
	}
	response, err := alice.Get(f.server.URL + "/listening/recent")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("播放日志不能被浏览器公共缓存")
	}
}
