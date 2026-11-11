package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

func sourceUpdateTestServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	s := newAdminStabilityServer(t)
	t.Cleanup(s.StopSourceUpdates)
	clock := &atomic.Int64{}
	clock.Store(time.Date(2026, 11, 11, 9, 0, 0, 0, sourceUpdateLocation).Unix())
	s.sourceUpdates().now = func() time.Time { return time.Unix(clock.Load(), 0) }
	return s, clock
}

func TestSourceURLImportDefaultsAndSettings(t *testing.T) {
	s, clock := sourceUpdateTestServer(t)
	script := "// @version 1\n" + countedAdminScript("imported")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "lxsc" {
			t.Error("下载未沿用服务标识")
		}
		_, _ = w.Write([]byte("\uFEFF" + script))
	}))
	defer upstream.Close()
	for _, body := range []string{
		`{"url":"` + upstream.URL + `","name":"管理员命名"}`,
		`{"url":"` + upstream.URL + `","autoUpdate":false}`,
	} {
		w := httptest.NewRecorder()
		s.importSource(w, httptest.NewRequest(http.MethodPost, "/sources/import", strings.NewReader(body)))
		var view sourceView
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
			t.Fatal("导入失败", w.Code, w.Body.String())
		}
		wantAuto := !strings.Contains(body, `"autoUpdate":false`)
		if view.URL != upstream.URL || view.AutoUpdate != wantAuto || view.LastCheckedAt != clock.Load() || view.ScriptUpdatedAt != clock.Load() || view.UpdateResult != "imported" || view.UpdateRunning {
			t.Fatalf("导入没有保存来源或默认更新规则: %+v", view)
		}
		stored, _ := s.DB.GetSource(context.Background(), view.ID)
		if stored.Script != script || sourceCheckDue(stored, s.sourceUpdates().now()) {
			t.Fatal("导入重复检查或没有归一化 BOM")
		}
		if wantAuto && view.NextCheckAt != time.Date(2026, 11, 12, 3, 0, 0, 0, sourceUpdateLocation).Unix() {
			t.Fatal("下次检查时间不为北京时间凌晨3点")
		}
	}
	source, _, err := s.AddSource(context.Background(), script, "原上传音源")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.updateSource(w, adminSourceRequest(source.ID, map[string]any{"url": upstream.URL, "autoUpdate": true}))
	configured, _ := s.DB.GetSource(context.Background(), source.ID)
	if w.Code != 200 || !configured.AutoUpdate || configured.URL != upstream.URL || configured.Name != source.Name || !configured.Enabled || configured.Script != script {
		t.Fatal("已有音源无法补填来源或原配置被改变")
	}
}

func TestSourceUpdateUsesContentAndPreservesCacheOnNoChange(t *testing.T) {
	s, clock := sourceUpdateTestServer(t)
	old := "// @version 1\n" + countedAdminScript("old")
	next := "// @version 1\n// @name 作者的新名称\n" + countedAdminScript("new")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(next)) }))
	defer upstream.Close()
	source, _, err := s.addSource(context.Background(), old, "管理员名称", upstream.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	info := music.FromMap(map[string]any{"source": "wy", "songmid": "one", "types": []any{map[string]any{"type": "320k"}}})
	first, err := s.Catalog.ResolveURL(context.Background(), info, "320k")
	if err != nil || !strings.Contains(first.URL, "/old/") {
		t.Fatal(err)
	}
	clock.Add(3600)
	updated, err := s.performSourceCheck(context.Background(), source.ID, false)
	if err != nil || updated.UpdateResult != "updated" || updated.Script != next || updated.Version != "1" || updated.Name != source.Name || !updated.Enabled || !updated.AutoUpdate || updated.URL != source.URL || updated.ScriptUpdatedAt != clock.Load() {
		t.Fatal("同版本内容变化未安全替换", updated, err)
	}
	result, err := s.Catalog.ResolveURL(context.Background(), info, "320k")
	if err != nil || !strings.Contains(result.URL, "/new/") || result.SourceID != source.ID {
		t.Fatal("更新后未生效或旧直链缓存未失效", result, err)
	}
	clock.Add(3600)
	unchanged, err := s.performSourceCheck(context.Background(), source.ID, false)
	if err != nil || unchanged.UpdateResult != "unchanged" || unchanged.ScriptUpdatedAt != updated.ScriptUpdatedAt || unchanged.LastCheckedAt != clock.Load() {
		t.Fatal("内容未变却伪造更新时间", unchanged, err)
	}
	cached, err := s.Catalog.ResolveURL(context.Background(), info, "320k")
	if err != nil || cached.URL != result.URL {
		t.Fatal("内容未变却清除了有效直链缓存")
	}
	uncached, err := s.Sources.MusicURLForSources(context.Background(), "wy", nil, "320k", []int64{source.ID})
	if err != nil || !strings.HasSuffix(uncached.URL, "/2") {
		t.Fatal("内容未变却重新加载了运行实例", uncached, err)
	}
}

func TestSourceUpdateFailuresKeepOldScriptAndWorker(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"下载失败", "", 503},
		{"空脚本", " ", 200},
		{"执行失败", `throw new Error('https://private.invalid/?token=PRIVATE');`, 200},
		{"初始化失败", `lx.send(lx.EVENT_NAMES.inited,{sources:null});`, 200},
		{"脚本过大", strings.Repeat("x", maxSourceScriptBytes+1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clock := sourceUpdateTestServer(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			source, _, err := s.addSource(context.Background(), countedAdminScript("old"), "旧音源", upstream.URL+"?token=PRIVATE", true)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = s.Sources.MusicURLForSources(context.Background(), "wy", nil, "320k", []int64{source.ID})
			clock.Add(3600)
			if _, err := s.performSourceCheck(context.Background(), source.ID, false); err == nil {
				t.Fatal("无效脚本更新成功")
			}
			stored, _ := s.DB.GetSource(context.Background(), source.ID)
			if stored.Script != source.Script || stored.ScriptUpdatedAt != source.ScriptUpdatedAt || stored.UpdateResult != "failed" || stored.UpdateAttempts != 1 || stored.RetryAt != clock.Load()+300 || strings.Contains(stored.UpdateError, "PRIVATE") || strings.Contains(stored.UpdateError, "private.invalid") {
				t.Fatal("失败修改了旧脚本、丢失重试状态或泄露原始错误")
			}
			result, err := s.Sources.MusicURLForSources(context.Background(), "wy", nil, "320k", []int64{source.ID})
			if err != nil || !strings.HasSuffix(result.URL, "/old/2") {
				t.Fatal("失败卸载或重载了旧运行实例", result, err)
			}
		})
	}
}

func TestSourceUpdateCalendarRetriesAndRuntimeRestart(t *testing.T) {
	s, clock := sourceUpdateTestServer(t)
	var downloads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downloads.Add(1); w.WriteHeader(503) }))
	defer upstream.Close()
	source, _, err := s.addSource(context.Background(), countedAdminScript("old"), "音源", upstream.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 12, 3, 0, 0, 0, sourceUpdateLocation).Unix()
	for _, step := range []struct {
		minutes            int64
		requests, attempts int32
	}{{0, 1, 1}, {4, 1, 1}, {5, 2, 2}, {34, 2, 2}, {35, 3, 3}, {800, 3, 3}} {
		clock.Store(base + step.minutes*60)
		_, _ = s.performSourceCheck(context.Background(), source.ID, true)
		stored, _ := s.DB.GetSource(context.Background(), source.ID)
		if downloads.Load() != step.requests || stored.UpdateAttempts != int(step.attempts) {
			t.Fatalf("重试未遵守5/30分钟与三次上限: %+v 请求%d", stored, downloads.Load())
		}
		if step.minutes == 0 {
			// 新调度运行时只使用数据库中保存的重试状态。
			s.StopSourceUpdates()
			s = &Server{DB: s.DB, Sources: s.Sources, Catalog: s.Catalog, HTTP: s.HTTP, Log: s.Log}
			s.sourceUpdates().now = func() time.Time { return time.Unix(clock.Load(), 0) }
			t.Cleanup(s.StopSourceUpdates)
		}
	}
	clock.Store(time.Date(2026, 11, 15, 9, 0, 0, 0, sourceUpdateLocation).Unix())
	_, _ = s.performSourceCheck(context.Background(), source.ID, true)
	stored, _ := s.DB.GetSource(context.Background(), source.ID)
	if downloads.Load() != 4 || stored.UpdateSlot != "2026-11-15" || stored.UpdateAttempts != 1 {
		t.Fatal("错过多天没有只补查最新一次")
	}
	_, _ = s.performSourceCheck(context.Background(), source.ID, true)
	if downloads.Load() != 4 {
		t.Fatal("同一到期计划重复执行")
	}
	for _, now := range []time.Time{time.Date(2026, 11, 16, 2, 59, 0, 0, sourceUpdateLocation), time.Date(2026, 11, 15, 18, 59, 0, 0, time.UTC)} {
		slot, next := sourcePlanSlot(now)
		if slot != "2026-11-15" || next.Unix() != time.Date(2026, 11, 16, 3, 0, 0, 0, sourceUpdateLocation).Unix() {
			t.Fatal("计划受进程时区影响")
		}
	}
}

func TestSourceUpdateCancelsForConfigurationChangesAndShutdown(t *testing.T) {
	for _, operation := range []string{"地址", "停用", "删除", "退出"} {
		t.Run(operation, func(t *testing.T) {
			s, clock := sourceUpdateTestServer(t)
			started, ended := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(ended) }))
			defer upstream.Close()
			source, _, err := s.addSource(context.Background(), countedAdminScript("old"), "旧音源", upstream.URL, true)
			if err != nil {
				t.Fatal(err)
			}
			clock.Add(24 * 3600)
			var stop func()
			done := make(chan error, 1)
			if operation == "退出" {
				stop = s.StartSourceUpdates(context.Background())
			} else {
				go func() { _, err := s.performSourceCheck(context.Background(), source.ID, false); done <- err }()
			}
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("更新没有开始")
			}
			w := httptest.NewRecorder()
			switch operation {
			case "地址":
				s.updateSource(w, adminSourceRequest(source.ID, map[string]any{"url": "https://new.invalid/source.js"}))
			case "停用":
				s.updateSource(w, adminSourceRequest(source.ID, map[string]any{"enabled": false}))
			case "删除":
				s.deleteSource(w, adminSourceRequest(source.ID, nil))
			case "退出":
				stop()
			}
			if operation != "退出" {
				if w.Code != 200 {
					t.Fatal("管理操作失败", w.Code)
				}
				select {
				case err := <-done:
					if !errors.Is(err, errSourceUpdateCancelled) {
						t.Fatal("在途更新未取消", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("更新取消后未退出")
				}
			}
			select {
			case <-ended:
			case <-time.After(2 * time.Second):
				t.Fatal("后台下载没有停止")
			}
			if s.sourceUpdates().control(source.ID).running.Load() {
				t.Fatal("结束后仍显示在途更新")
			}
			stored, err := s.DB.GetSource(context.Background(), source.ID)
			if operation == "删除" {
				if !errors.Is(err, db.ErrNotFound) {
					t.Fatal("音源未删除")
				}
			} else if err != nil || stored.Script != source.Script || stored.ScriptUpdatedAt != source.ScriptUpdatedAt {
				t.Fatal("取消后旧脚本被修改")
			}
		})
	}
}
