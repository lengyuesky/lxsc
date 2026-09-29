package subsonic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/settings"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestPaginationAndLimits(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "review.db"))
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
	server := &Server{DB: database, Catalog: catalog, Settings: store, Log: log}
	infos := []*music.Info{}
	for i := 0; i < 3; i++ {
		infos = append(infos, music.FromMap(map[string]any{"source": "wy", "songmid": fmt.Sprint(i), "name": "测试歌曲", "singer": "测试歌手", "albumName": "测试专辑"}))
	}
	if err := catalog.RememberSync(ctx, infos); err != nil {
		t.Fatal(err)
	}
	request := func(offset int) string {
		req := httptest.NewRequest("GET", fmt.Sprintf("/rest/search3.view?query=local:%%E6%%B5%%8B%%E8%%AF%%95&songCount=1&songOffset=%d&f=json", offset), nil)
		req = req.WithContext(withUser(req.Context(), user))
		rec := httptest.NewRecorder()
		server.search(rec, req)
		var body map[string]map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body["subsonic-response"]["searchResult3"].(map[string]any)["song"].([]any)[0].(map[string]any)["id"].(string)
	}
	t.Run("本地搜索第二页不能重复第一页", func(t *testing.T) {
		a, b := request(0), request(1)
		if a == b {
			t.Errorf("offset=0 与 offset=1 均返回 %s", a)
		}
	})
	t.Run("随机歌曲负数大小不能触发恐慌", func(t *testing.T) {
		defer func() {
			if v := recover(); v != nil {
				t.Errorf("size=-1 触发 panic: %v", v)
			}
		}()
		req := httptest.NewRequest("GET", "/rest/getRandomSongs.view?size=-1&f=json", nil)
		req = req.WithContext(withUser(req.Context(), user))
		server.getRandomSongs(httptest.NewRecorder(), req)
	})
}

func TestPaginationNumericBounds(t *testing.T) {
	for _, tc := range []struct {
		value string
		count int
	}{{"-1", 0}, {"0", 0}, {"501", 500}, {"9223372036854775807", 500}, {"12garbage", 20}, {"not-a-number", 20}} {
		req := httptest.NewRequest("GET", "/?count="+tc.value+"&offset="+tc.value, nil)
		if got := paramCount(req, "count", 20); got != tc.count {
			t.Errorf("数量 %q 解析为 %d，期望 %d", tc.value, got, tc.count)
		}
		offset := paramOffset(req, "offset")
		if offset < 0 || offset > int(^uint(0)>>1)-500 {
			t.Fatal("偏移超出安全范围", offset)
		}
	}
	list := []int{1, 2, 3}
	for _, tc := range []struct {
		offset, count int
		want          int
	}{{0, 0, 0}, {0, -1, 0}, {-1, 1, 0}, {2, 100, 1}, {int(^uint(0) >> 1), 1, 0}, {1, int(^uint(0) >> 1), 2}} {
		if got := slicePage(list, tc.offset, tc.count); len(got) != tc.want {
			t.Fatal("分页边界错误", tc, got)
		}
	}
}
