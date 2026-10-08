package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/webauth"
)

func TestSourceStatisticsRequireAdminAndValidateWindow(t *testing.T) {
	s := newAdminStabilityServer(t)
	s.Auth = &webauth.Manager{DB: s.DB}
	ctx := context.Background()
	for _, admin := range []bool{false, true} {
		name := "普通用户"
		if admin {
			name = "管理员"
		}
		user, err := s.DB.CreateUser(ctx, name, "", admin, "320k")
		if err != nil {
			t.Fatal(err)
		}
		cookie := httptest.NewRecorder()
		s.Auth.Start(cookie, httptest.NewRequest("GET", "http://test/", nil), user)
		for _, window := range []string{"", "1h", "24h", "7d", "0", "-1"} {
			req := httptest.NewRequest(http.MethodGet, "/sources/statistics?window="+window, nil)
			req.AddCookie(cookie.Result().Cookies()[0])
			w := httptest.NewRecorder()
			s.Routes().ServeHTTP(w, req)
			want := http.StatusOK
			if !admin {
				want = http.StatusForbidden
			} else if window != "" && window != "1h" && window != "24h" {
				want = http.StatusBadRequest
			}
			if w.Code != want {
				t.Fatalf("角色或时间范围错误: 管理员=%v 范围=%q 状态=%d 内容=%s", admin, window, w.Code, w.Body)
			}
			if want == http.StatusOK && (!strings.Contains(w.Body.String(), `"sources":[]`) || w.Header().Get("Cache-Control") != "no-store") {
				t.Fatal("空数据或缓存策略错误", w.Body)
			}
		}
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sources/statistics", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatal("未登录应拒绝访问")
	}
}

func TestSourceStatisticsCacheHitsAndSourceLifecycle(t *testing.T) {
	s := newAdminStabilityServer(t)
	ctx := context.Background()
	source, _, err := s.AddSource(ctx, countedAdminScript("secret-signed-url"), "统计音源", 10)
	if err != nil {
		t.Fatal(err)
	}
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "name": "统计歌曲", "types": []any{map[string]any{"type": "320k"}}})
	for range 3 {
		if _, err := s.Catalog.ResolveURL(ctx, info, "320k"); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []sourceStatisticsView {
		t.Helper()
		w := httptest.NewRecorder()
		s.sourceStatistics(w, httptest.NewRequest(http.MethodGet, "/sources/statistics?window=24h", nil))
		var body struct {
			Sources []sourceStatisticsView `json:"sources"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
			t.Fatal("统计响应错误", err, w.Body)
		}
		if strings.Contains(w.Body.String(), "secret-signed-url") || strings.Contains(w.Body.String(), "lx.send") {
			t.Fatal("统计返回了播放地址或脚本")
		}
		return body.Sources
	}
	items := read()
	if len(items) != 1 || items[0].ID != source.ID || items[0].Statistics.Summary.Calls != 1 || items[0].Statistics.Recent[0].Song != "统计歌曲" {
		t.Fatalf("缓存命中不应重复计数: %+v", items)
	}
	for _, action := range []string{"停用", "启用", "重载"} {
		w := httptest.NewRecorder()
		if action == "重载" {
			s.reloadSource(w, adminSourceRequest(source.ID, nil))
		} else {
			s.updateSource(w, adminSourceRequest(source.ID, map[string]any{"enabled": action == "启用"}))
		}
		if w.Code != 200 || read()[0].Statistics.Summary.Calls != 1 {
			t.Fatal("音源状态变更应保留统计", action, w.Body)
		}
		if action == "停用" && read()[0].State != "disabled" {
			t.Fatal("应显示已停用并保留历史")
		}
	}
	w := httptest.NewRecorder()
	s.deleteSource(w, adminSourceRequest(source.ID, nil))
	if w.Code != 200 || len(read()) != 0 || s.Sources.CallStatisticsOf(source.ID, time.Now(), time.Hour).Total.Calls != 0 {
		t.Fatal("删除音源后未清理统计")
	}
}

func TestSourceTestDoesNotUseOtherScripts(t *testing.T) {
	s := newAdminStabilityServer(t)
	ctx := context.Background()
	first, _, err := s.AddSource(ctx, countedAdminScript("first"), "首选音源", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.AddSource(ctx, `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});globalThis.__lx_request=()=>{throw new Error('仅此音源失败')}`, "测试音源", 2)
	if err != nil {
		t.Fatal(err)
	}
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := js.NewSDKPool(1, string(prelude), `globalThis.__sdk_call=()=>({list:[{source:'wy',songmid:'one',name:'测试歌曲'}]})`, s.HTTP, s.HTTP, s.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s.Catalog.SDK = pool
	w := httptest.NewRecorder()
	s.testSource(w, adminSourceRequest(second.ID, map[string]any{"platform": "wy", "quality": "320k"}))
	var result map[string]struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result["wy"].OK {
		t.Fatal("测试失败音源时不能借用首选音源显示成功", w.Body, err)
	}
	if got := s.Sources.CallStatisticsOf(first.ID, time.Now(), time.Hour); got.Total.Calls != 0 {
		t.Fatal("测试调用错误归属到首选音源")
	}
	if got := s.Sources.CallStatisticsOf(second.ID, time.Now(), time.Hour); got.Total.Calls != 1 || got.Total.Failed != 1 {
		t.Fatalf("测试结果未计入所选音源: %+v, 响应=%s", got.Total, w.Body)
	}
}
