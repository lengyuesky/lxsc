package music

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"lxsc/internal/db"
)

func TestParseOnlinePlaylistID(t *testing.T) {
	for _, tc := range []struct{ source, input, want string }{
		{"wy", " 123456 ", "123456"},
		{"wy", "分享歌单 https://music.163.com/#/playlist?id=123&userid=456 分享给你", "123"},
		{"wy", "https://music.163.com/playlist?id=123", "123"},
		{"wy", "https://y.music.163.com/m/playlist?id=123&app_version=1", "123"},
		{"wy", "https://music.163.com/playlist/123/456/", "123"},
		{"tx", "123", "123"},
		{"tx", "https://y.qq.com/n/ryqq/playlist/123", "123"},
		{"tx", "https://y.qq.com/n/yqq/playlist/123.html", "123"},
		{"tx", "https://y.qq.com/n/yqq/playsquare/123.html", "123"},
		{"tx", "分享 https://i.y.qq.com/n/m/detail/taoge/index.html?id=123&ADTAG=test", "123"},
		{"tx", "https://y.qq.com/n/ryqq/playlist?disstid=123", "123"},
		{"tx", "https://i.y.qq.com/n2/m/share/details/taoge.html?platform=11&id=123&ADTAG=qfshare", "123"},
		{"wy", "https://y.qq.com/n/ryqq/playlist/123", ""},
		{"tx", "https://music.163.com/playlist?id=123", ""},
		{"wy", "https://music.163.com.evil.test/playlist?id=123", ""},
		{"wy", "http://127.0.0.1/playlist?id=123", ""},
		{"wy", "https://evil.test@music.163.com/playlist?id=123", ""},
		{"wy", "https://music.163.com:8080/playlist?id=123", ""},
		{"wy", "https://music.163.com/song?id=123", ""},
		{"tx", "https://y.qq.com/n/ryqq/albumDetail/123", ""},
		{"tx", "https://c6.y.qq.com/base/fcgi-bin/u?__=abcdef", ""},
		{"wy", "file:///playlist?id=123", ""},
		{"wy", "-1", ""},
		{"wy", "000", ""},
		{"wy", strings.Repeat("1", 21), ""},
		{"wy", "https://music.163.com/playlist?id=123###token", "123"},
		{"kw", "123", ""},
	} {
		t.Run(tc.source+"/"+tc.input, func(t *testing.T) {
			got, err := ParseOnlinePlaylistID(tc.source, tc.input)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("解析结果不符: got=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func playlistPage(t *testing.T, name string, total, limit int, list []map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"info": map[string]any{"name": name}, "total": total, "limit": limit, "list": list})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func onlineSong(id int) map[string]any {
	return map[string]any{"songmid": fmt.Sprint(id), "name": fmt.Sprintf("歌曲%d", id), "singer": "歌手", "albumName": "专辑"}
}

func TestFetchOnlinePlaylistPaginationAndPreview(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	calls := 0
	c.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		calls++
		if path != "wy.songList.getListDetail" || len(args) != 2 || args[0] != "123" || args[1] != calls {
			t.Errorf("网易云参数不符: %s %v", path, args)
		}
		if calls == 1 {
			return playlistPage(t, "测试歌单", 5, 3, []map[string]any{onlineSong(1), onlineSong(1), nil}), nil
		}
		return playlistPage(t, "测试歌单", 5, 3, []map[string]any{onlineSong(2), onlineSong(3)}), nil
	})
	pl, err := c.FetchOnlinePlaylist(context.Background(), "wy", "123")
	if err != nil || calls != 2 || len(pl.Tracks) != 3 || pl.Skipped != 1 || pl.Name != "测试歌单" {
		t.Fatalf("分页结果异常: %+v calls=%d err=%v", pl, calls, err)
	}
	for i, in := range pl.Tracks {
		if in.TrackID() != fmt.Sprintf("tr-wy-%d", i+1) {
			t.Fatalf("顺序或来源异常: %+v", in)
		}
		if _, err := c.DB.GetTrack(context.Background(), in.TrackID()); !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("预览不得落库: %v", err)
		}
	}
	if _, err := c.FetchOnlinePlaylist(context.Background(), "wy", "123"); err != nil || calls != 2 {
		t.Fatalf("确认导入应复用预览缓存: %v calls=%d", err, calls)
	}
}

func TestFetchOnlinePlaylistQQArgumentsAndLimit(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	c.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		if path != "tx.songList.getListDetail" || len(args) != 1 {
			t.Errorf("QQ 不得传页码作为重试次数: %s %v", path, args)
		}
		list := make([]map[string]any, 2005)
		for i := range list {
			list[i] = onlineSong(i + 1)
		}
		return playlistPage(t, "QQ 歌单", len(list), len(list)+1, list), nil
	})
	pl, err := c.FetchOnlinePlaylist(context.Background(), "tx", "123")
	if err != nil || len(pl.Tracks) != 2000 || pl.Truncated != 5 || pl.Tracks[0].Source() != "tx" {
		t.Fatalf("QQ 导入结果异常: %+v err=%v", pl, err)
	}
}

func TestFetchOnlinePlaylistNeteaseBounded(t *testing.T) {
	c := newDirectoryTestCatalog(t)
	calls := 0
	c.SetRemoteCallerForTest(func(_ context.Context, _ string, args ...any) (json.RawMessage, error) {
		calls++
		page := args[1].(int)
		list := make([]map[string]any, 1000)
		for i := range list {
			list[i] = onlineSong((page-1)*1000 + i + 1)
		}
		return playlistPage(t, "大歌单", 5000, 1000, list), nil
	})
	pl, err := c.FetchOnlinePlaylist(context.Background(), "wy", "123")
	if err != nil || calls != 2 || len(pl.Tracks) != 2000 || pl.Truncated != 3000 {
		t.Fatalf("分页上限异常: %+v calls=%d err=%v", pl, calls, err)
	}
}

func TestFetchOnlinePlaylistRejectsIncompleteResults(t *testing.T) {
	for _, kind := range []string{"异常结构", "空页", "重复分页", "后续失败", "总数变化"} {
		t.Run(kind, func(t *testing.T) {
			c := newDirectoryTestCatalog(t)
			calls := 0
			c.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
				calls++
				if kind == "异常结构" {
					return json.RawMessage(`{"code":403}`), nil
				}
				if calls == 1 {
					return playlistPage(t, "列表", 3, 2, []map[string]any{onlineSong(1), onlineSong(2)}), nil
				}
				switch kind {
				case "空页":
					return playlistPage(t, "列表", 3, 2, []map[string]any{}), nil
				case "重复分页":
					return playlistPage(t, "列表", 3, 2, []map[string]any{onlineSong(1), onlineSong(2)}), nil
				case "总数变化":
					return playlistPage(t, "列表", 4, 2, []map[string]any{onlineSong(3)}), nil
				default:
					return nil, errors.New("平台失败")
				}
			})
			if pl, err := c.FetchOnlinePlaylist(context.Background(), "wy", "123"); err == nil || pl != nil {
				t.Fatalf("不完整结果不能当成功: %+v %v", pl, err)
			}
		})
	}
}
