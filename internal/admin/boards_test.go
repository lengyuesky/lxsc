package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBoardChoicesAreCompleteAndOnlyLoadNames(t *testing.T) {
	s := newAdminStabilityServer(t)
	ctx := context.Background()
	if _, err := s.Settings.Update(ctx, map[string]json.RawMessage{"showBoards": json.RawMessage(`false`), "boardSources": json.RawMessage(`[]`), "boardSelections": json.RawMessage(`{"wy":[]}`)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, _ ...any) (json.RawMessage, error) {
		calls++
		if path != "wy.leaderboard.getBoards" {
			t.Errorf("不应加载歌曲: %s", path)
		}
		return json.RawMessage(`{"list":[{"id":"wy__one","name":"榜单一"},{"id":2,"name":"榜单二","bangId":2}]}`), nil
	})
	before, err := s.DB.Statistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		rec := httptest.NewRecorder()
		s.getBoards(rec, httptest.NewRequest(http.MethodGet, "/boards?source=wy", nil))
		var boards []struct {
			Source string `json:"source"`
			BangID string `json:"bangid"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &boards); err != nil || rec.Code != 200 || len(boards) != 2 {
			t.Fatalf("候选目录不能受展示设置影响: %d %s %v", rec.Code, rec.Body.String(), err)
		}
		if boards[0].Source != "wy" || boards[0].BangID != "one" || boards[1].BangID != "2" {
			t.Fatalf("应返回规范化后的榜单标识: %+v", boards)
		}
	}
	if calls != 1 {
		t.Fatalf("应复用已有目录缓存: %d", calls)
	}
	after, err := s.DB.Statistics(ctx)
	if err != nil || before.Tracks != after.Tracks || before.Albums != after.Albums || before.Artists != after.Artists {
		t.Fatalf("读取候选项不能持久化音乐元数据: %+v %+v %v", before, after, err)
	}
}

func TestBoardChoiceFailuresAndValidation(t *testing.T) {
	s := newAdminStabilityServer(t)
	s.Catalog.SetRemoteCallerForTest(func(_ context.Context, _ string, _ ...any) (json.RawMessage, error) {
		return nil, fmt.Errorf("模拟平台不可用")
	})
	for _, source := range []string{"", "other"} {
		rec := httptest.NewRecorder()
		s.getBoards(rec, httptest.NewRequest(http.MethodGet, "/boards?source="+source, nil))
		if rec.Code != 400 {
			t.Fatalf("无效平台必须返回 400: %d", rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	s.getBoards(rec, httptest.NewRequest(http.MethodGet, "/boards?source=wy", nil))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "重试") {
		t.Fatalf("平台失败应返回明确错误，不能伪装成空榜单: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.putSettings(rec, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(`{"boardSelections":{"wy":null},"serverName":"不应修改"}`)))
	if rec.Code != 400 || s.Settings.Get().ServerName != "lxsc" {
		t.Fatalf("非法选择应返回 400 并拒绝整个更新: %d %s", rec.Code, rec.Body.String())
	}
}
