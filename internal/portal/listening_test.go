package portal

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"lxsc/internal/db"
)

func TestListeningAPIIsolationAndValidation(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.client(t, "alice")
	bob := f.client(t, "bob")
	admin := f.client(t, "admin")
	p := db.ListeningProgress{UserID: f.users["alice"].ID, SessionID: "portal-session", TrackID: "tr-wy-1", StartedAt: time.Now().UnixMilli(), Days: map[string]int64{time.Now().In(db.ListeningZone).Format("2006-01-02"): 0}}
	for _, client := range []*http.Client{bob, admin} {
		if code, _ := doJSON(t, client, "POST", f.server.URL+"/listening/progress", p); code != 403 {
			t.Fatalf("账号错配应返回403，实际%d", code)
		}
	}
	if code, _ := doJSON(t, alice, "POST", f.server.URL+"/listening/progress", p); code != 200 {
		t.Fatalf("合法心跳失败%d", code)
	}
	for _, path := range []string{"?scope=all", "?scope=user&userId=" + strconv.FormatInt(f.users["bob"].ID, 10)} {
		if code, _ := doJSON(t, alice, "GET", f.server.URL+"/listening/stats"+path, nil); code != 403 {
			t.Fatalf("不能越权查看%s：%d", path, code)
		}
		if code, _ := doJSON(t, admin, "GET", f.server.URL+"/listening/stats"+path, nil); code != 200 {
			t.Fatalf("管理员查询失败%s：%d", path, code)
		}
	}
	for _, path := range []string{"?days=31", "?days=no", "?scope=unknown", "?scope=me&userId=1"} {
		if code, _ := doJSON(t, alice, "GET", f.server.URL+"/listening/stats"+path, nil); code != 400 {
			t.Fatalf("无效范围%s返回%d", path, code)
		}
	}
	if code, _ := doJSON(t, http.DefaultClient, "GET", f.server.URL+"/listening/stats", nil); code != 401 {
		t.Fatal("统计必须登录")
	}
	at := time.Now()
	if err := f.db.AddClientListening(context.Background(), f.users["bob"].ID, "客户端", db.ListeningTrack{ID: "tr-wy-1", Name: "歌曲"}, 90000, at.UnixMilli(), true, at); err != nil {
		t.Fatal(err)
	}
	if code, stats := doJSON(t, alice, "GET", f.server.URL+"/listening/stats", nil); code != 200 || stats["totalMs"] != float64(0) {
		t.Fatalf("其他用户时长泄露：%v", stats)
	}
	if code, stats := doJSON(t, admin, "GET", f.server.URL+"/listening/stats?scope=all", nil); code != 200 || stats["totalMs"] != float64(90000) {
		t.Fatalf("全站统计错误：%v", stats)
	}
}
