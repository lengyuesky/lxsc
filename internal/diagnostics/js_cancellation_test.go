package diagnostics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/assets"
	"lxsc/internal/js"
	"lxsc/internal/music"
)

// requestCache 在登记等待者后才读取 Done；借此确定正常播放已加入同一取链。
type joinedContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *joinedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestProbeCancellationOwnsSourceHTTPWithoutCancellingSharedPlayback(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared_%t", shared), func(t *testing.T) {
			f := newFixture(t)
			started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/first" {
					_, _ = io.WriteString(w, "first")
					return
				}
				if requests.Add(1) == 1 {
					close(started)
				}
				select {
				case <-r.Context().Done():
					close(cancelled)
				case <-release:
					_, _ = io.WriteString(w, "ready")
				}
			}))
			defer upstream.Close()
			defer close(release)
			// 同时经过定时器、async/await、Promise和原生HTTP回调，不能只传递同步调用栈。
			script := fmt.Sprintf(`
function fetchPart(path) { return new Promise((resolve,reject)=>lx.request(%q+path,{},(err)=>err?reject(err):resolve())); }
lx.on(lx.EVENT_NAMES.request, async ()=> {
 await new Promise(resolve=>setTimeout(resolve,1));
 await fetchPart('/first');
 await fetchPart('/wait');
 return 'https://media.example/audio';
});
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{name:'合成音源',type:'music',actions:['musicUrl'],qualitys:['320k']}}});`, upstream.URL)
			setupProbeScript(t, f, script)
			view, key := f.create(t, true)
			probe := make(chan int, 1)
			go func() {
				probe <- f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil).Code
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("JS取链HTTP未启动")
			}
			playback := make(chan error, 1)
			if shared {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				joined := &joinedContext{Context: ctx, joined: make(chan struct{})}
				info, err := f.s.Catalog.Track(ctx, "tr-wy-1")
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					result, err := f.s.Catalog.ResolvePlaybackURL(joined, info, "320k")
					if err == nil && result.Result.URL != "https://media.example/audio" {
						err = fmt.Errorf("正常播放未得到链接")
					}
					playback <- err
				}()
				select {
				case <-joined.joined:
				case <-time.After(time.Second):
					t.Fatal("正常播放未加入共享取链")
				}
			}
			f.s.Tokens.Revoke(view.ID)
			select {
			case code := <-probe:
				if code != 408 {
					t.Fatal(code)
				}
			case <-time.After(time.Second):
				t.Fatal("probe未随撤销结束")
			}
			if !shared {
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("最后等待者离开后JS HTTP仍在运行")
				}
			} else {
				select {
				case <-cancelled:
					t.Fatal("撤销probe误取消正常播放的共享HTTP")
				case <-time.After(100 * time.Millisecond):
				}
				release <- struct{}{}
				select {
				case err := <-playback:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("正常播放未完成")
				}
			}
			if requests.Load() != 1 {
				t.Fatal("未合并取链请求", requests.Load())
			}
		})
	}
}

func TestProbeRevocationCancelsSDKMetadataHTTP(t *testing.T) {
	f := newFixture(t)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-release:
		}
	}))
	defer upstream.Close()
	defer close(release)
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bundle := fmt.Sprintf(`async function __sdk_info() { await new Promise((resolve,reject)=>lx.request(%q,{},err=>err?reject(err):resolve())); return {source:'wy',songmid:'1'}; }`, upstream.URL)
	sdk, err := js.NewSDKPool(1, string(prelude), bundle, &http.Client{}, &http.Client{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer sdk.Close()
	f.s.Catalog = music.NewCatalog(f.s.DB, sdk, nil, f.s.Settings, log)
	view, key := f.create(t, true)
	result := make(chan int, 1)
	go func() {
		result <- f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil).Code
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("SDK元数据HTTP未启动")
	}
	f.s.Tokens.Revoke(view.ID)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("撤销未取消SDK HTTP")
	}
	select {
	case code := <-result:
		if code != 408 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("元数据探测未结束")
	}
}
