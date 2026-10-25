package js

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
)

func TestCallContextInterruptedHTTPCallbackKeepsWorkerAlive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ready")
	}))
	defer upstream.Close()
	worker := newTestWorker(t)
	entered := make(chan struct{}, 1)
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	if err := worker.runSync(context.Background(), func(vm *goja.Runtime) error {
		return vm.Set("__bufferCallbackStarted", func() {
			entered <- struct{}{}
			<-resume
		})
	}); err != nil {
		t.Fatal(err)
	}
	// 明确阻塞在 HTTP 回调的 Buffer.from 内，取消后再恢复执行；不依赖随机调度或短超时。
	if err := worker.RunScript(context.Background(), "interrupt-buffer-test.js", `
var __originalBufferFrom = Buffer.from;
var __responseDelivered = false;
Buffer.from = function() {
  __bufferCallbackStarted();
  return __originalBufferFrom.apply(this, arguments);
};`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := worker.Eval(ctx, fmt.Sprintf(`new Promise((resolve,reject)=>__host.fetch({url:%q},(err,res)=>{
__responseDelivered = true;
if (err) reject(err); else resolve(res.raw.toString());
}))`, upstream.URL))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP 响应未进入 Buffer 回调")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("必须保留调用取消原因", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消调用未返回")
	}
	release()
	// 该执行排在中断的回调之后，确认事件循环仍可用，取消结果没有被交付。
	checkCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	out, err := worker.Eval(checkCtx, `Buffer.from = __originalBufferFrom; [__responseDelivered, Buffer.from('ready').toString()]`)
	if err != nil || string(out) != `[false,"ready"]` {
		t.Fatalf("中断不能终止事件循环或污染后续调用: %s %v", out, err)
	}
	out, err = worker.Eval(checkCtx, fmt.Sprintf(`new Promise((resolve,reject)=>__host.fetch({url:%q},(err,res)=>err?reject(err):resolve(res.raw.toString())))`, upstream.URL))
	if err != nil || string(out) != `"ready"` {
		t.Fatalf("中断后 HTTP 应继续正常工作: %s %v", out, err)
	}
	// 结果在回调内交付；排队屏障确保回调已退出，再检查额度释放。
	if err := worker.runSync(checkCtx, func(*goja.Runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if stats := fetchLimits.Stats(); stats.Active != 0 || stats.Queued != 0 {
		t.Fatalf("中断回调必须归还全部 HTTP 额度: %+v", stats)
	}
}
