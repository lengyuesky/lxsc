package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func boardPageJSON(total, limit int, ids ...int) json.RawMessage {
	list := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		list = append(list, map[string]any{"songmid": fmt.Sprint(id), "name": "测试歌曲", "interval": "00:03"})
	}
	result := map[string]any{"list": list, "limit": limit}
	if total >= 0 {
		result["total"] = total
	}
	raw, _ := json.Marshal(result)
	return raw
}

func TestFullBoardHasNoPlaylistSizeCapAndReusesCompleteCache(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	calls := 0
	const total = 2505
	c.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		calls++
		if path != "kg.leaderboard.getList" {
			t.Errorf("意外请求 %s", path)
		}
		page := args[1].(int)
		ids := []int{}
		for i := (page-1)*100 + 1; i <= min(page*100, total); i++ {
			ids = append(ids, i)
		}
		return boardPageJSON(total, 100, ids...), nil
	})
	before, _ := c.DB.Statistics(context.Background())
	list, err := c.FullBoardTracks(context.Background(), "kg", "8888")
	if err != nil || len(list) != total || calls != 26 {
		t.Fatalf("整榜被截断: count=%d calls=%d err=%v", len(list), calls, err)
	}
	for i, track := range list {
		if track.Key() != fmt.Sprint(i+1) {
			t.Fatalf("排名顺序变化: %d %s", i, track.Key())
		}
	}
	n, duration, ok := c.CachedBoardSummary("kg", "8888")
	if !ok || n != total || duration != 3*total {
		t.Fatal(n, duration, ok)
	}
	again, err := c.FullBoardTracks(context.Background(), "kg", "8888")
	if err != nil || len(again) != total || calls != 26 {
		t.Fatal("完整缓存没有复用", err, calls)
	}
	after, _ := c.DB.Statistics(context.Background())
	if before.Tracks != after.Tracks || before.Albums != after.Albums || before.Artists != after.Artists {
		t.Fatal("浏览榜单不能持久化歌曲资料")
	}
}

func TestFullBoardRejectsPartialAndRepeatedPages(t *testing.T) {
	for _, kind := range []string{"失败", "提前空页", "重复页", "总数变化", "页码错误", "异常结构"} {
		t.Run(kind, func(t *testing.T) {
			c := newDirectoryTestCatalog(t)
			c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
				if args[1].(int) == 1 {
					return boardPageJSON(3, 2, 1, 2), nil
				}
				switch kind {
				case "失败":
					return nil, errors.New("上游失败")
				case "提前空页":
					return boardPageJSON(3, 2), nil
				case "重复页":
					return boardPageJSON(3, 2, 1, 2), nil
				case "总数变化":
					return boardPageJSON(4, 2, 3), nil
				case "页码错误":
					return json.RawMessage(`{"list":[{"songmid":"3"}],"total":3,"limit":2,"page":1}`), nil
				default:
					return json.RawMessage(`{"error":"failed"}`), nil
				}
			})
			list, err := c.FullBoardTracks(context.Background(), "kw", "16")
			if err == nil || list != nil {
				t.Fatalf("部分榜单不能作为成功返回: %v %v", list, err)
			}
			if _, _, ok := c.CachedBoardSummary("kw", "16"); ok {
				t.Fatal("第一页数量不能冒充完整缓存")
			}
		})
	}
}

func TestFullBoardSupportsSDKPaginationVariants(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pages []json.RawMessage
		want  []string
	}{
		{"一次返回全部", []json.RawMessage{boardPageJSON(5, 2, 1, 2, 3, 4, 5)}, []string{"1", "2", "3", "4", "5"}},
		{"总数未知", []json.RawMessage{boardPageJSON(-1, 2, 1, 2), boardPageJSON(-1, 2, 3)}, []string{"1", "2", "3"}},
		{"过滤不可用歌曲", []json.RawMessage{boardPageJSON(4, 2, 1), boardPageJSON(4, 2, 3)}, []string{"1", "3"}},
		{"相邻页重复歌曲去重", []json.RawMessage{boardPageJSON(4, 2, 1, 2), boardPageJSON(4, 2, 2, 3)}, []string{"1", "2", "3"}},
		{"空榜单", []json.RawMessage{boardPageJSON(0, 100)}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newDirectoryTestCatalog(t)
			calls := 0
			c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
				calls++
				p := args[1].(int)
				if p > len(tc.pages) {
					return nil, errors.New("不应读取更多分页")
				}
				return tc.pages[p-1], nil
			})
			list, err := c.FullBoardTracks(context.Background(), "kg", "8888")
			if err != nil {
				t.Fatal(err)
			}
			keys := []string{}
			for _, in := range list {
				keys = append(keys, in.Key())
			}
			if !reflect.DeepEqual(keys, tc.want) || calls != len(tc.pages) {
				t.Fatal(keys, calls)
			}
		})
	}
}
