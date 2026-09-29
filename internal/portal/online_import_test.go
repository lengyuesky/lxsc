package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"lxsc/internal/db"
)

const onlineImportSample = `{"list":[
 {"songmid":"101","name":"歌曲一","singer":"歌手","albumName":"专辑","types":[{"type":"320k"}]},
 {"songmid":"101","name":"歌曲一"},
 {"songmid":"102","name":"歌曲二"},null],"total":4,"limit":1000,"info":{"name":"在线歌单"}}`

func TestOnlineImportPreviewCreateAppend(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.client(t, "alice")
	admin := f.client(t, "admin")
	base := f.server.URL + "/playlists/import/online"
	var calls atomic.Int32
	f.service.Catalog.SetRemoteCallerForTest(func(_ context.Context, path string, args ...any) (json.RawMessage, error) {
		calls.Add(1)
		if path == "tx.songList.getListDetail" && len(args) != 1 {
			t.Error("QQ 接口参数错误")
		}
		return json.RawMessage(onlineImportSample), nil
	})
	status, out := doJSON(t, alice, http.MethodPost, base, map[string]any{"source": "wy", "input": "https://music.163.com/#/playlist?id=123", "preview": true})
	if status != 200 || len(out["lists"].([]any)) != 1 || out["lists"].([]any)[0].(map[string]any)["count"] != float64(2) {
		t.Fatalf("预览失败: %d %+v", status, out)
	}
	if playlists, _ := f.db.ListAllPlaylists(context.Background(), 0); len(playlists) != 0 {
		t.Fatal("预览不应创建歌单")
	}
	if _, err := f.db.GetTrack(context.Background(), "tr-wy-101"); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("预览不应保存歌曲")
	}
	status, out = doJSON(t, alice, http.MethodPost, base, map[string]any{"source": "wy", "input": "123", "public": true})
	if status != 200 || out["added"] != float64(2) || out["skipped"] != float64(1) || calls.Load() != 1 {
		t.Fatalf("创建失败或未复用缓存: %d %+v calls=%d", status, out, calls.Load())
	}
	created := out["playlists"].([]any)[0].(map[string]any)
	id := created["id"].(string)
	p, _ := f.db.GetPlaylist(context.Background(), id)
	if p.UserID != f.users["alice"].ID || !p.Public || p.Name != "在线歌单" || !strings.Contains(p.Comment, "music.163.com") || !reflect.DeepEqual(p.TrackIDs, []string{"tr-wy-101", "tr-wy-102"}) {
		t.Fatalf("歌单信息错误: %+v", p)
	}
	track, err := f.db.GetTrack(context.Background(), "tr-wy-101")
	if err != nil || !strings.Contains(string(track.JSON), "320k") {
		t.Fatalf("播放元数据丢失: %+v %v", track, err)
	}
	status, out = doJSON(t, alice, http.MethodPost, base, map[string]any{"source": "wy", "input": "123", "target": id, "expectedRevision": db.TracksRevision(p.TrackIDs)})
	if status != 200 || out["added"] != float64(0) {
		t.Fatalf("重复追加应去重: %d %+v", status, out)
	}
	status, out = doJSON(t, alice, http.MethodPost, base, map[string]any{"source": "tx", "input": "123", "target": id})
	if status != 200 || out["added"] != float64(2) {
		t.Fatalf("跨平台追加失败: %d %+v", status, out)
	}
	p, _ = f.db.GetPlaylist(context.Background(), id)
	if !reflect.DeepEqual(p.TrackIDs, []string{"tr-wy-101", "tr-wy-102", "tr-tx-101", "tr-tx-102"}) {
		t.Fatalf("追加必须保留原有顺序: %+v", p.TrackIDs)
	}
	status, out = doJSON(t, admin, http.MethodPost, base, map[string]any{"source": "tx", "input": "123", "ownerId": f.users["bob"].ID, "public": false})
	if status != 200 || out["playlists"].([]any)[0].(map[string]any)["userId"] != float64(f.users["bob"].ID) {
		t.Fatalf("管理员代建失败: %d %+v", status, out)
	}
}

func TestOnlineImportRejectsBeforeUpstream(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.client(t, "alice")
	ctx := context.Background()
	if err := f.db.CreatePlaylist(ctx, "pl-bob", f.users["bob"].ID, "Bob 歌单", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreatePlaylist(ctx, "pl-alice", f.users["alice"].ID, "Alice 歌单", nil); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	f.service.Catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		calls.Add(1)
		return json.RawMessage(onlineImportSample), nil
	})
	base := f.server.URL + "/playlists/import/online"
	if status, _ := doJSON(t, http.DefaultClient, http.MethodPost, base, map[string]any{"source": "wy", "input": "123"}); status != 401 {
		t.Fatalf("未登录应被拒绝: %d", status)
	}
	for _, tc := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"source": "wy", "input": "123", "target": "pl-bob"}, 403},
		{map[string]any{"source": "wy", "input": "123", "ownerId": f.users["bob"].ID}, 403},
		{map[string]any{"source": "wy", "input": "123", "target": "pl-missing"}, 404},
		{map[string]any{"source": "wy", "input": "123", "target": "pl-alice", "expectedRevision": "旧版本"}, 409},
		{map[string]any{"source": "wy", "input": "http://127.0.0.1/playlist?id=123"}, 400},
		{map[string]any{"source": "wy", "input": "https://y.qq.com/n/ryqq/playlist/123"}, 400},
		{map[string]any{"source": "kg", "input": "123"}, 400},
		{map[string]any{"source": "wy", "input": "123", "cookie": "秘密"}, 400},
	} {
		if status, out := doJSON(t, alice, http.MethodPost, base, tc.body); status != tc.code {
			t.Fatalf("请求应在上游前拒绝: %+v got=%d want=%d %+v", tc.body, status, tc.code, out)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("不合法请求不得访问上游: %d", calls.Load())
	}
}

func TestOnlineImportAppendLimit(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.client(t, "alice")
	ctx := context.Background()
	ids := make([]string, maxPlaylistTracks-1)
	for i := range ids {
		ids[i] = fmt.Sprintf("tr-wy-old-%d", i)
	}
	if err := f.db.CreatePlaylist(ctx, "pl-nearly-full", f.users["alice"].ID, "即将满额", ids); err != nil {
		t.Fatal(err)
	}
	f.service.Catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
		return json.RawMessage(onlineImportSample), nil
	})
	status, out := doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists/import/online", map[string]any{"source": "wy", "input": "123", "target": "pl-nearly-full"})
	if status != 200 || out["added"] != float64(1) || out["truncated"] != float64(1) {
		t.Fatalf("追加上限错误: %d %+v", status, out)
	}
	p, _ := f.db.GetPlaylist(ctx, "pl-nearly-full")
	if !reflect.DeepEqual(p.TrackIDs, append(ids, "tr-wy-101")) {
		t.Fatal("上限截断不能改变原有歌曲顺序")
	}
	if _, err := f.db.GetTrack(ctx, "tr-wy-102"); !errors.Is(err, db.ErrNotFound) {
		t.Fatal("被截断的歌曲不得落库")
	}
	status, out = doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists/import/online", map[string]any{"source": "wy", "input": "123", "target": "pl-nearly-full"})
	if status != 200 || out["added"] != float64(0) || out["truncated"] != float64(1) {
		t.Fatalf("满额重复追加错误: %d %+v", status, out)
	}
}

func TestOnlineImportFailureAndConcurrentChange(t *testing.T) {
	for _, kind := range []string{"上游失败", "空歌单", "并发追加"} {
		t.Run(kind, func(t *testing.T) {
			f := newPortalFixture(t)
			alice := f.client(t, "alice")
			ctx := context.Background()
			if err := f.db.CreatePlaylist(ctx, "pl-target", f.users["alice"].ID, "目标", []string{"tr-wy-1"}); err != nil {
				t.Fatal(err)
			}
			f.service.Catalog.SetRemoteCallerForTest(func(context.Context, string, ...any) (json.RawMessage, error) {
				switch kind {
				case "上游失败":
					return nil, errors.New("平台不可访问")
				case "空歌单":
					return json.RawMessage(`{"list":[],"total":0,"info":{"name":"空"}}`), nil
				default:
					if err := f.db.ReplacePlaylistTracks(ctx, "pl-target", []string{"tr-wy-1", "tr-tx-2"}); err != nil {
						return nil, err
					}
					return json.RawMessage(onlineImportSample), nil
				}
			})
			status, out := doJSON(t, alice, http.MethodPost, f.server.URL+"/playlists/import/online", map[string]any{"source": "wy", "input": "123", "target": "pl-target"})
			want := map[string]int{"上游失败": 502, "空歌单": 400, "并发追加": 409}[kind]
			if status != want {
				t.Fatalf("状态不符: %d %+v", status, out)
			}
			if _, err := f.db.GetTrack(ctx, "tr-wy-101"); !errors.Is(err, db.ErrNotFound) {
				t.Fatal("失败导入不得留下歌曲元数据")
			}
			p, _ := f.db.GetPlaylist(ctx, "pl-target")
			wantIDs := []string{"tr-wy-1"}
			if kind == "并发追加" {
				wantIDs = append(wantIDs, "tr-tx-2")
			}
			if !reflect.DeepEqual(p.TrackIDs, wantIDs) {
				t.Fatalf("失败导入覆盖了原歌单: %+v", p)
			}
		})
	}
}
