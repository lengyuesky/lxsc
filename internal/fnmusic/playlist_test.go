package fnmusic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

func playlistFixture(t *testing.T, count int) (*fixture, string, string, []string) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	infos := make([]*music.Info, 0, count)
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		track := music.FromMap(map[string]any{
			"source": "wy", "songmid": fmt.Sprintf("playlist-%03d", i),
			"name": fmt.Sprintf("私密合成曲目%03d", i), "singer": "私密合成歌手",
			"albumName": "合成专辑", "interval": "00:45",
		})
		infos = append(infos, track)
		ids = append(ids, track.TrackID())
	}
	if err := f.server.Catalog.RememberSync(ctx, infos); err != nil {
		t.Fatal(err)
	}
	const id = "pl-synthetic-large"
	if err := f.db.CreatePlaylistFull(ctx, id, f.users["alice"].ID, "私密合成歌单", "私密合成备注", false, ids); err != nil {
		t.Fatal(err)
	}
	guid, err := f.server.identify(ctx, "playlist", id)
	if err != nil {
		t.Fatal(err)
	}
	return f, login(t, f, "alice"), guid, ids
}

func playlistWriteDiagnostic(t *testing.T, f *fixture, w *httptest.ResponseRecorder, endpoint string) diagnostics.Event {
	t.Helper()
	list := f.server.Diagnostics.List()
	if len(list) == 0 {
		t.Fatal("缺少歌单响应诊断")
	}
	event := list[len(list)-1]
	if event.Stage != "client_response" || event.Client != "fnmusic" || event.Endpoint != endpoint || event.Method != http.MethodGet || event.Format != "json" || event.Status != w.Code {
		t.Fatalf("歌单请求归类错误: %+v", event)
	}
	if event.RequestID == "" || event.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatal("诊断请求 ID 与响应头不一致")
	}
	if event.BytesWritten == nil || event.ResponseBytes == nil || *event.BytesWritten != int64(w.Body.Len()) || *event.ResponseBytes != int64(w.Body.Len()) {
		t.Fatalf("歌单响应写出计数错误: %+v", event)
	}
	return event
}

func TestPlaylistTracksUnpagedAndExplicitPagination(t *testing.T) {
	for _, total := range []int{330, 641} {
		t.Run(fmt.Sprintf("%d首", total), func(t *testing.T) {
			f, token, guid, ids := playlistFixture(t, total)
			endpoint := "/music/api/v1/track/playlist-detail/list?playlistGUID=" + guid
			for _, tc := range []struct {
				name, params string
				start, end   int
				page, size   int
				descending   bool
			}{
				{"无分页", "", 0, total, 0, 0, false},
				{"语言参数", "&lan=zh-CN", 0, total, 0, 0, false},
				{"空分页参数", "&page=&size=", 0, total, 0, 0, false},
				{"原版客户端读取全部", "&page=1&size=-1&sort=&lan=zh-CN", 0, total, 1, -1, false},
				{"全量标记不按页切片", "&page=9&size=-1", 0, total, 1, -1, false},
				{"显式第二页", "&page=2&size=100", 100, 200, 2, 100, false},
				{"仅指定页码", "&page=2", 100, 200, 2, 100, false},
				{"第七页", "&page=7&size=100", min(600, total), min(700, total), 7, 100, false},
				{"保留每页上限", "&page=1&size=5000", 0, min(500, total), 1, 500, false},
				{"无效分页使用默认值", "&page=invalid&size=0", 0, 100, 1, 100, false},
				{"其他负数沿用默认分页", "&page=1&size=-2", 0, 100, 1, 100, false},
				{"越界页", "&page=999&size=100", total, total, 999, 100, false},
				{"无分页排序", "&sort=title,desc", 0, total, 0, 0, true},
				{"全量排序", "&page=1&size=-1&sort=title,desc", 0, total, 1, -1, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					w := request(f, http.MethodGet, endpoint+tc.params, token, nil)
					data := payload(t, w)
					items := data["list"].([]any)
					if data["total"] != float64(total) || len(items) != tc.end-tc.start {
						t.Fatalf("总数%v，返回%d首，预期总数%d、返回%d首", data["total"], len(items), total, tc.end-tc.start)
					}
					for i, item := range items {
						position := tc.start + i
						if tc.descending {
							position = total - position - 1
						}
						if item.(map[string]any)["guid"] != identifier("track", ids[position]) {
							t.Fatalf("第%d首重复、遗漏或顺序错误", i)
						}
					}
					event := playlistWriteDiagnostic(t, f, w, "getPlaylist")
					if event.Count == nil || *event.Count != len(items) || event.Total == nil || *event.Total != total {
						t.Fatalf("曲目诊断计数错误: %+v", event)
					}
					wantResult := "ok"
					if len(items) == 0 {
						wantResult = "empty"
					}
					if event.Result != wantResult || event.Error != "none" {
						t.Fatalf("曲目诊断结果错误: %+v", event)
					}
					if tc.page == 0 {
						if event.Page != nil || event.PageSize != nil {
							t.Fatal("无分页请求被记录为显式分页")
						}
					} else if event.Page == nil || *event.Page != tc.page || event.PageSize == nil || *event.PageSize != tc.size {
						t.Fatalf("有效分页参数错误: %+v", event)
					}
				})
			}
			library := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list", token, nil))
			if len(library["list"].([]any)) != 100 || library["total"] != float64(total+2) {
				t.Fatal("音乐库列表的默认分页发生变化")
			}
			allLibrary := payload(t, request(f, http.MethodGet, "/music/api/v1/track/list?page=1&size=-1", token, nil))
			if len(allLibrary["list"].([]any)) != total+2 || allLibrary["total"] != float64(total+2) {
				t.Fatal("音乐库列表未保留客户端显式读取全部的语义")
			}
			raw, _ := json.Marshal(f.server.Diagnostics.List())
			for _, private := range []string{token, guid, "pl-synthetic-large", "alice", "私密合成歌单", "私密合成曲目", "私密合成歌手", "私密合成备注"} {
				if bytes.Contains(raw, []byte(private)) {
					t.Fatal("歌单诊断包含私密文本或凭据")
				}
			}
		})
	}
}

func TestPlaylistResponseDiagnosticsAndAccess(t *testing.T) {
	f, token, guid, _ := playlistFixture(t, 330)
	for _, tc := range []struct {
		path        string
		count, page int
	}{
		{"/music/api/v1/playlist/list", 3, 0},
		{"/music/api/v1/playlist/list?page=2&size=1", 1, 2},
	} {
		w := request(f, http.MethodGet, tc.path, token, nil)
		data := payload(t, w)
		if data["total"] != float64(3) || len(data["list"].([]any)) != tc.count {
			t.Fatal("歌单列表数量错误")
		}
		event := playlistWriteDiagnostic(t, f, w, "getPlaylists")
		if event.Count == nil || *event.Count != tc.count || event.Total == nil || *event.Total != 3 {
			t.Fatalf("歌单列表诊断数量错误: %+v", event)
		}
		if tc.page == 0 {
			if event.Page != nil || event.PageSize != nil {
				t.Fatal("无分页歌单列表包含分页标记")
			}
		} else if event.Page == nil || *event.Page != tc.page || event.PageSize == nil || *event.PageSize != 1 {
			t.Fatal("歌单列表分页诊断错误")
		}
	}
	w := request(f, http.MethodGet, "/music/api/v1/playlist/detail?guid="+guid, token, nil)
	if payload(t, w)["trackCount"] != float64(330) {
		t.Fatal("歌单摘要数量错误")
	}
	event := playlistWriteDiagnostic(t, f, w, "getPlaylist")
	if event.Count != nil || event.Total != nil || event.Result != "ok" {
		t.Fatal("歌单摘要冒充完整曲目列表")
	}
	private, err := f.server.identify(context.Background(), "playlist", "pl-bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/music/api/v1/playlist/detail?guid=" + private,
		"/music/api/v1/track/playlist-detail/list?playlistGUID=" + private,
	} {
		w = request(f, http.MethodGet, path, token, nil)
		if w.Code != http.StatusNotFound {
			t.Fatal("其他用户的私有歌单被放行")
		}
		event = playlistWriteDiagnostic(t, f, w, "getPlaylist")
		if event.Result != "failed" || event.Count != nil || event.Total != nil {
			t.Fatal("无权限请求返回了歌单数量")
		}
	}
}

type playlistShortWriter struct{ *httptest.ResponseRecorder }

func (w *playlistShortWriter) Write(data []byte) (int, error) {
	return w.ResponseRecorder.Write(data[:len(data)/2])
}

func TestPlaylistWriteFailuresAreDiagnosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wrap  func(*httptest.ResponseRecorder) http.ResponseWriter
		short bool
	}{
		{"刷新失败", func(w *httptest.ResponseRecorder) http.ResponseWriter { return &resourceFlushWriter{w} }, false},
		{"短写", func(w *httptest.ResponseRecorder) http.ResponseWriter { return &playlistShortWriter{w} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, token, guid, _ := playlistFixture(t, 330)
			r := httptest.NewRequest(http.MethodGet, "/music/api/v1/track/playlist-detail/list?playlistGUID="+guid, nil)
			r.AddCookie(&http.Cookie{Name: "music-token", Value: token})
			w := httptest.NewRecorder()
			f.routes.ServeHTTP(tc.wrap(w), r)
			list := f.server.Diagnostics.List()
			event := list[len(list)-1]
			if event.Result != "failed" || event.Error != "write_error" || event.Count == nil || *event.Count != 330 || event.Total == nil || *event.Total != 330 || event.BytesWritten == nil || event.ResponseBytes == nil {
				t.Fatalf("歌单写出失败被记为成功: %+v", event)
			}
			if (*event.BytesWritten < *event.ResponseBytes) != tc.short {
				t.Fatal("未区分短写与正文写完后的刷新失败")
			}
			raw, _ := json.Marshal(list)
			for _, private := range []string{token, guid, "私密合成歌单", "私密合成曲目", "synthetic private network message"} {
				if bytes.Contains(raw, []byte(private)) {
					t.Fatal("写出失败诊断泄露私密数据")
				}
			}
		})
	}
}
