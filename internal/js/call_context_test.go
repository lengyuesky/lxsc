package js

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCallContextCancelsOnlyItsOwnHTTP(t *testing.T) {
	started := make(chan string, 2)
	cancelled := make(chan string, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		select {
		case <-r.Context().Done():
			cancelled <- r.URL.Path
		case <-release:
			_, _ = io.WriteString(w, "ready")
		}
	}))
	defer upstream.Close()
	defer close(release)
	worker := newTestWorker(t)
	script := fmt.Sprintf(`function requestPart(path) { return new Promise((resolve,reject)=>lx.request(%q+path,{},(err)=>err?reject(err):resolve(path))); }
async function owned(path) { await new Promise(resolve=>setImmediate(resolve)); return await requestPart(path); }`, upstream.URL)
	if err := worker.RunScript(context.Background(), "owned-test.js", script); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := worker.CallJSON(ctx, "owned", "/cancel"); first <- err }()
	go func() { _, err := worker.CallJSON(context.Background(), "owned", "/keep"); second <- err }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("两个调用未同时发起HTTP")
		}
	}
	cancel()
	select {
	case path := <-cancelled:
		if path != "/cancel" {
			t.Fatal("误取消其他调用", path)
		}
	case <-time.After(time.Second):
		t.Fatal("调用取消未传递到HTTP")
	}
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("取消调用成功返回")
		}
	case <-time.After(time.Second):
		t.Fatal("取消调用未返回")
	}
	select {
	case path := <-cancelled:
		t.Fatal("Worker全局取消", path)
	case <-time.After(50 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case err := <-second:
		if err != nil {
			t.Fatal("其他调用受影响", err)
		}
	case <-time.After(time.Second):
		t.Fatal("其他调用无法完成")
	}
}

func TestCallContextCancelsScheduledHTTP(t *testing.T) {
	requests := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests <- struct{}{} }))
	defer upstream.Close()
	worker := newTestWorker(t)
	script := fmt.Sprintf(`function delayed() { return new Promise(resolve=>setTimeout(()=>lx.request(%q,{},resolve),100)); }`, upstream.URL)
	if err := worker.RunScript(context.Background(), "timer-test.js", script); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := worker.CallJSON(ctx, "delayed"); err == nil {
		t.Fatal("调用应超时")
	}
	select {
	case <-requests:
		t.Fatal("取消后定时器仍创建HTTP")
	case <-time.After(150 * time.Millisecond):
	}
}
