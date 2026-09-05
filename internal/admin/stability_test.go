package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/assets"
	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/settings"
)

func newAdminStabilityServer(t *testing.T) *Server {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store, err := settings.New(context.Background(), database)
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
	t.Cleanup(sources.UnloadAll)
	catalog := music.NewCatalog(database, nil, sources, store, log)
	return &Server{DB: database, Catalog: catalog, Sources: sources, Settings: store, Log: log, HTTP: client}
}

func countedAdminScript(tag string) string {
	return fmt.Sprintf(`
let count = 0
lx.on(lx.EVENT_NAMES.request, () => Promise.resolve('https://cdn.example/%s/' + (++count)))
lx.send(lx.EVENT_NAMES.inited, {status:true, sources:{wy:{name:'测试',type:'music',actions:['musicUrl'],qualitys:['320k']}}})`, tag)
}

func adminSourceRequest(id int64, body any) *http.Request {
	encoded, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/sources/"+strconv.FormatInt(id, 10), strings.NewReader(string(encoded)))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", strconv.FormatInt(id, 10))
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestSourceMutationsInvalidateURLs(t *testing.T) {
	for _, action := range []string{"新增", "改名", "优先级", "脚本", "停用", "启用", "重载", "删除", "重载失败", "删除失败"} {
		t.Run(action, func(t *testing.T) {
			s := newAdminStabilityServer(t)
			ctx := context.Background()
			first, _, err := s.AddSource(ctx, countedAdminScript("first"), "首选", 100)
			if err != nil {
				t.Fatal(err)
			}
			var second *db.Source
			if action == "优先级" {
				second, _, err = s.AddSource(ctx, countedAdminScript("second"), "候选", 200)
				if err != nil {
					t.Fatal(err)
				}
			}
			info := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "types": []any{map[string]any{"type": "320k"}}})
			if _, err := s.Catalog.ResolveURL(ctx, info, "320k"); err != nil {
				t.Fatal(err)
			}
			// 缓存第二次结果，重载后计数重置，便于区分是否命中旧缓存。
			s.Catalog.InvalidateURLs()
			before, err := s.Catalog.ResolveURL(ctx, info, "320k")
			if err != nil {
				t.Fatal(err)
			}
			if before.URL != "https://cdn.example/first/2" {
				t.Fatalf("预热错误: %s", before.URL)
			}
			rec := httptest.NewRecorder()
			switch action {
			case "新增":
				_, _, err = s.AddSource(ctx, countedAdminScript("new"), "新增", 50)
				if err != nil {
					t.Fatal(err)
				}
			case "改名":
				s.updateSource(rec, adminSourceRequest(first.ID, map[string]any{"name": "改名"}))
			case "优先级":
				s.updateSource(rec, adminSourceRequest(second.ID, map[string]any{"priority": 50}))
			case "脚本":
				s.updateSource(rec, adminSourceRequest(first.ID, map[string]any{"script": countedAdminScript("updated")}))
			case "停用":
				s.updateSource(rec, adminSourceRequest(first.ID, map[string]any{"enabled": false}))
			case "启用":
				// 模拟已有停用记录与遗留缓存，启用后必须重新取链。
				s.Sources.Unload(first.ID)
				first.Enabled = false
				if err := s.DB.UpdateSource(ctx, first); err != nil {
					t.Fatal(err)
				}
				s.updateSource(rec, adminSourceRequest(first.ID, map[string]any{"enabled": true}))
			case "重载":
				s.reloadSource(rec, adminSourceRequest(first.ID, nil))
			case "删除":
				s.deleteSource(rec, adminSourceRequest(first.ID, nil))
			case "重载失败":
				first.Script = "throw new Error('模拟加载失败')"
				if err := s.DB.UpdateSource(ctx, first); err != nil {
					t.Fatal(err)
				}
				s.reloadSource(rec, adminSourceRequest(first.ID, nil))
			case "删除失败":
				if err := s.DB.Close(); err != nil {
					t.Fatal(err)
				}
				s.deleteSource(rec, adminSourceRequest(first.ID, nil))
			}
			if action == "删除失败" {
				if rec.Code != 500 {
					t.Fatal("数据库错误应返回500")
				}
			} else if rec.Code != 200 {
				t.Fatalf("操作失败: %d %s", rec.Code, rec.Body.String())
			}
			after, err := s.Catalog.ResolveURL(ctx, info, "320k")
			switch action {
			case "停用", "删除", "重载失败", "删除失败":
				if err == nil {
					t.Fatalf("无可用音源时不得继续使用缓存: %v", after)
				}
			case "改名":
				if err != nil || after.URL != before.URL {
					t.Fatal("仅改名不应清空缓存")
				}
			default:
				if err != nil || after.URL == before.URL {
					t.Fatalf("音源变更必须失效旧直链: %v %v", after, err)
				}
			}
		})
	}
}

func TestSettingsInvalidTTLReturns400(t *testing.T) {
	s := newAdminStabilityServer(t)
	req := httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(`{"urlCacheTTL":1.5,"serverName":"不应生效"}`))
	rec := httptest.NewRecorder()
	s.putSettings(rec, req)
	if rec.Code != 400 || s.Settings.Get().URLCacheTTL != 900 || s.Settings.Get().ServerName != "lxsc" {
		t.Fatalf("非法更新应原子拒绝: %d %s", rec.Code, rec.Body.String())
	}
}
