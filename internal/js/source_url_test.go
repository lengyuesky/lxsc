package js

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const startupSourcePrelude = `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['320k']}}});`

func startupSources(t *testing.T, scripts ...string) *SourceManager {
	t.Helper()
	m := NewSourceManager(loadAsset(t, "prelude.js"), &http.Client{}, &http.Client{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(m.UnloadAll)
	for index, script := range scripts {
		if _, err := m.Load(context.Background(), int64(index+1), index, startupSourcePrelude+script); err != nil {
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

func TestMusicURLFastPrimaryDoesNotStartBackup(t *testing.T) {
	m := startupSources(t,
		`globalThis.__lx_request=()=>({url:'https://media.invalid/primary'});`,
		`globalThis.__lx_request=()=>({url:'https://media.invalid/backup'});`,
	)
	result, err := m.MusicURL(context.Background(), "wy", nil, "320k")
	if err != nil || result.SourceID != 1 {
		t.Fatalf("快速首选源应保持优先: %+v %v", result, err)
	}
	if got := m.CallStatisticsOf(2, time.Now(), time.Hour); got.Total.Calls != 0 || got.InFlight != 0 {
		t.Fatal("首选源及时成功时不得额外请求备用源")
	}
}

func TestMusicURLSlowPrimaryUsesBackupAndCancelsHTTP(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			started <- struct{}{}
			<-r.Context().Done()
			cancelled <- struct{}{}
			return
		}
		_, _ = io.WriteString(w, "ready")
	}))
	t.Cleanup(server.Close)
	m := startupSources(t, startupRequestScript(server.URL+"/slow", false), startupRequestScript(server.URL+"/fast", false))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	done := make(chan error, 1)
	begin := time.Now()
	go func() {
		result, err := m.MusicURL(ctx, "wy", nil, "320k")
		if err == nil && result.SourceID != 2 {
			err = errors.New("未采用快速备用源")
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("首选源未开始")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(musicURLFallbackDelay + 2*time.Second):
		t.Fatal("备用源不应等待首选源耗尽十几秒预算")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("成功取链后必须取消慢源 HTTP")
	}
	if primary := m.CallStatisticsOf(1, time.Now(), time.Hour); primary.InFlight != 0 || primary.Total.Cancelled != 1 {
		t.Fatalf("慢源必须释放并记录为取消: %+v", primary.Total)
	}
	t.Logf("慢源未响应、备用源正常：取链用时 %s", time.Since(begin).Round(time.Millisecond))
}

func TestMusicURLBackupFailureKeepsPrimaryAlive(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/primary" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		} else {
			close(release)
		}
		_, _ = io.WriteString(w, "ready")
	}))
	t.Cleanup(server.Close)
	m := startupSources(t, startupRequestScript(server.URL+"/primary", false), startupRequestScript(server.URL+"/backup", true))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := m.MusicURL(ctx, "wy", nil, "320k")
	if err != nil || result.SourceID != 1 {
		t.Fatalf("备用失败时不能放弃仍可能成功的首选源: %+v %v", result, err)
	}
}

func TestMusicURLLimitsParallelSourcesAndCancelsAll(t *testing.T) {
	started, cancelled := make(chan string, 3), make(chan string, 3)
	var active, maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); count > old && !maximum.CompareAndSwap(old, count); old = maximum.Load() {
		}
		started <- r.URL.Path
		<-r.Context().Done()
		cancelled <- r.URL.Path
	}))
	t.Cleanup(server.Close)
	m := startupSources(t, startupRequestScript(server.URL+"/a", false), startupRequestScript(server.URL+"/b", false), startupRequestScript(server.URL+"/c", false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.MusicURL(ctx, "wy", nil, "320k"); done <- err }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(musicURLFallbackDelay + time.Second):
			t.Fatal("应依次启动首选和一个备用")
		}
	}
	select {
	case path := <-started:
		t.Fatal("两个源在途时不能启动第三个", path)
	case <-time.After(musicURLFallbackDelay + 20*time.Millisecond):
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
	for range 2 {
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("所有在途 HTTP 都必须取消")
		}
	}
	if maximum.Load() != 2 || m.CallStatisticsOf(3, time.Now(), time.Hour).Total.Calls != 0 {
		t.Fatal("并行音源数不能超过两个，取消后不得继续尝试其他源")
	}
}
