package js

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const startupSourcePrelude = `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`

func startupSources(t *testing.T, scripts ...string) *SourceManager {
	t.Helper()
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	for index, script := range scripts {
		if _, err := m.Load(context.Background(), int64(index+1), startupSourcePrelude+script); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func startupRequestScript(address string, fail bool) string {
	return fmt.Sprintf(`globalThis.__lx_request=()=>new Promise((resolve,reject)=>lx.request(%q,{},err=>{
if(err || %t) reject(new Error('合成取链失败')); else resolve({url:'https://media.invalid/ready'});
}));`, address, fail)
}

// 多音源取链逐个进行：前一个失败才请求下一个，返回第一个取得的地址。
func TestMusicURLTriesSourcesInOrderAndReturnsFirstURL(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, "ready")
	}))
	t.Cleanup(server.Close)
	m := startupSources(t,
		startupRequestScript(server.URL+"/a", true),
		startupRequestScript(server.URL+"/b", false),
		startupRequestScript(server.URL+"/c", false),
	)
	result, err := m.MusicURL(context.Background(), "wy", nil, "320k")
	if err != nil || result.SourceID != 2 || fmt.Sprint(paths) != "[/a /b]" {
		t.Fatalf("应按顺序逐个请求并在第一个成功处停止: %+v %v %v", result, err, paths)
	}
	if calls := m.CallStatisticsOf(3, time.Now(), time.Hour).Total.Calls; calls != 0 {
		t.Fatal("已取得地址后不应再请求后面的音源", calls)
	}
}

// 播放选源逐档取链并自行验证音频，因此可以关闭音源内的降档。
func TestExactMusicURLDisablesInSourceDowngrade(t *testing.T) {
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	script := `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k']}}});
globalThis.__lx_request=({info})=>{if(info.type==='flac')throw new Error('当前音质不可用');return {url:'https://media.invalid/'+info.type}};`
	if _, err := m.Load(context.Background(), 1, script); err != nil {
		t.Fatal(err)
	}
	result, err := m.MusicURL(context.Background(), "wy", nil, "flac")
	if err != nil || result.Quality != "320k" {
		t.Fatalf("默认在音源内向下降档: %+v %v", result, err)
	}
	before := m.CallStatisticsOf(1, time.Now(), time.Hour).Total.Attempts
	if _, err := m.MusicURL(WithExactMusicURL(context.Background()), "wy", nil, "flac"); err == nil {
		t.Fatal("指定音质取不到时不能悄悄换成更低音质")
	}
	if attempts := m.CallStatisticsOf(1, time.Now(), time.Hour).Total.Attempts - before; attempts != 1 {
		t.Fatalf("指定音质只调用脚本一次: %d", attempts)
	}
	if result, err := m.MusicURL(WithExactMusicURL(context.Background()), "wy", nil, "320k"); err != nil || result.Quality != "320k" {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestMusicURLCancellationStopsHTTPAndLaterSources(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	t.Cleanup(server.Close)
	m := startupSources(t, startupRequestScript(server.URL+"/a", false), startupRequestScript(server.URL+"/b", false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.MusicURL(ctx, "wy", nil, "320k"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("首个音源未开始")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("必须保留客户端取消原因", err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后取链未结束")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("在途 HTTP 必须取消")
	}
	if first, second := m.CallStatisticsOf(1, time.Now(), time.Hour), m.CallStatisticsOf(2, time.Now(), time.Hour); first.InFlight != 0 || second.Total.Calls != 0 {
		t.Fatalf("取消后必须收回额度且不再尝试其它音源: %+v %+v", first.Total, second.Total)
	}
}
