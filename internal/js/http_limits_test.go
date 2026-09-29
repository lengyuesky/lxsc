package js

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPResponseBoundaryAndChunkedOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, strings.Repeat("a", 1025))
			return
		}
		_, _ = io.WriteString(w, strings.Repeat("a", 1024))
	}))
	defer server.Close()
	w := &Worker{opts: Options{HTTP: server.Client(), MaxBodyBytes: 1024}}
	_, data, err := w.doFetch(context.Background(), "GET", server.URL, nil, nil, 0, false)
	if err != nil || len(data) != 1024 {
		t.Fatalf("边界大小应完整返回: %v", err)
	}
	response, data, err := w.doFetch(context.Background(), "GET", server.URL+"/large", nil, nil, 0, false)
	if !errors.Is(err, ErrResponseTooLarge) || data != nil || response != nil || errCode(err) != "ERESPONSETOOLARGE" {
		t.Fatalf("分块传输超限必须明确报错: %v", err)
	}
}

func TestScriptHTTPBurstIsBoundedAndRecovers(t *testing.T) {
	var active, peak atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		select {
		case <-release:
			_, _ = io.WriteString(w, "ok")
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	w := newTestWorker(t)
	url, _ := json.Marshal(server.URL)
	expression := fmt.Sprintf(`Promise.all(Array.from({length:50},()=>new Promise(resolve=>__host.fetch({url:%s,timeout:5000},(err,res)=>resolve(err ? err.code : 'ok')))))`, url)
	type result struct {
		data json.RawMessage
		err  error
	}
	done := make(chan result, 1)
	beforeRejected := fetchLimits.Stats().Rejected
	go func() { data, err := w.Eval(context.Background(), expression); done <- result{data, err} }()
	deadline := time.After(2 * time.Second)
	for fetchLimits.Stats().Queued != 32 || fetchLimits.Stats().Rejected-beforeRejected != 10 {
		select {
		case <-deadline:
			close(release)
			t.Fatal("脚本突发请求未进入有界队列")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	var codes []string
	if err := json.Unmarshal(out.data, &codes); err != nil {
		t.Fatal(err)
	}
	var success, busy int
	for _, code := range codes {
		if code == "ok" {
			success++
		}
		if code == "EBUSY" {
			busy++
		}
	}
	if success != 40 || busy != 10 || peak.Load() > 8 {
		t.Fatalf("HTTP 并发或拒绝数错误: 成功=%d 繁忙=%d 峰值=%d", success, busy, peak.Load())
	}
	if got := fetchLimits.Stats(); got.Active != 0 || got.Queued != 0 {
		t.Fatalf("请求结束后未释放名额: %+v", got)
	}
}

func TestWorkerStopReleasesGlobalRequestSlots(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { started <- struct{}{}; <-r.Context().Done() }))
	defer server.Close()
	w := newTestWorker(t)
	url, _ := json.Marshal(server.URL)
	done := make(chan error, 1)
	go func() {
		_, err := w.Eval(context.Background(), fmt.Sprintf(`new Promise((resolve,reject)=>__host.fetch({url:%s},(err,res)=>err?reject(err):resolve('ok')))`, url))
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("请求未开始")
	}
	w.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("停止后应取消调用")
		}
	case <-time.After(time.Second):
		t.Fatal("停止 Worker 后调用未取消")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fetchLimits.Stats().Active == 0 && callLimits.Stats().Active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("卸载 Worker 后全局并发额度未释放")
}
