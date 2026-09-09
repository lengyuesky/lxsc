package js

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestMusicURLSourceIdentityAndSelection(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, log)
	t.Cleanup(manager.UnloadAll)
	for _, item := range []struct {
		id       int64
		priority int
		label    string
	}{{51, 2, "b"}, {9, 1, "a"}} {
		script := `// @name 同名音源
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k','128k']}}});
globalThis.__lx_request=({info})=>{
  if ('` + item.label + `'==='a' && info.type!=='128k') throw new Error('该音质不可用');
  return {url:'https://media.invalid/` + item.label + `/'+info.type,type:'伪造音质',source:'伪造名字',sourceId:999,SourceID:999};
};`
		if _, err := manager.Load(context.Background(), item.id, item.priority, script); err != nil {
			t.Fatal(err)
		}
	}
	if got := manager.MusicURLSourceIDs("wy"); !reflect.DeepEqual(got, []int64{9, 51}) {
		t.Fatalf("真实候选ID必须遵循优先级：%v", got)
	}
	for _, tc := range []struct {
		ids     []int64
		id      int64
		quality string
		url     string
	}{
		{nil, 9, "128k", "a/128k"},
		{[]int64{51, 9}, 9, "128k", "a/128k"},
		{[]int64{51}, 51, "flac", "b/flac"},
	} {
		result, err := manager.MusicURLForSources(context.Background(), "wy", map[string]any{"songmid": "one"}, "flac", tc.ids)
		if err != nil || result.SourceID != tc.id || result.Quality != tc.quality || result.Source != "同名音源" || result.URL != "https://media.invalid/"+tc.url {
			t.Fatalf("脚本不能伪造身份，排除集合也不能改变源内降级：%+v %v", result, err)
		}
		raw, err := json.Marshal(result)
		if err != nil || strings.Contains(string(raw), "SourceID") || strings.Contains(string(raw), "sourceId") || strings.Contains(string(raw), "999") {
			t.Fatal("内部脚本身份不能新增到对外JSON", string(raw), err)
		}
	}
	if _, err := manager.MusicURLForSources(context.Background(), "wy", nil, "320k", []int64{}); !errors.Is(err, ErrNoSource) {
		t.Fatalf("空的剩余集合不能退回全部音源：%v", err)
	}
	if _, err := manager.MusicURLForSources(context.Background(), "wy", nil, "320k", []int64{999}); !errors.Is(err, ErrNoSource) {
		t.Fatalf("不能按脚本伪造的ID命中音源：%v", err)
	}
}
