// Package js 提供基于 goja 的 JS 运行环境，用于加载洛雪音源脚本与各平台 SDK。
package js

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/buffer"
	"github.com/dop251/goja_nodejs/console"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/dop251/goja_nodejs/process"
	"github.com/dop251/goja_nodejs/require"
	"github.com/dop251/goja_nodejs/url"
)

// Options 创建 Worker 的参数
type Options struct {
	Name          string
	Logger        *slog.Logger
	HTTP          *http.Client // 校验证书的客户端
	HTTPInsecure  *http.Client // 跳过证书校验的客户端
	Prelude       string       // 前置脚本（构造 lx 与全局对象）
	MaxBodyBytes  int64        // 上游响应体上限
	OnInited      func(payload string)
	OnUpdateAlert func(payload string)
	OnConsole     func(level, msg string)
}

// Worker 是单个 goja 运行时 + 事件循环，所有 JS 执行都在事件循环 goroutine 内进行
type Worker struct {
	opts    Options
	loop    *eventloop.EventLoop
	log     *slog.Logger
	running atomic.Bool
	stopped atomic.Bool
	vmRef   atomic.Pointer[goja.Runtime]
	// 由事件循环初始化后写入，只在循环内读取
	jsonParse goja.Callable
	await     goja.Callable
}

type consolePrinter struct{ w *Worker }

func (p consolePrinter) Log(s string)   { p.w.consoleOut("info", s) }
func (p consolePrinter) Warn(s string)  { p.w.consoleOut("warn", s) }
func (p consolePrinter) Error(s string) { p.w.consoleOut("error", s) }

func (w *Worker) consoleOut(level, msg string) {
	if w.opts.OnConsole != nil {
		w.opts.OnConsole(level, msg)
	}
	switch level {
	case "error":
		w.log.Error("[js] " + msg)
	case "warn":
		w.log.Warn("[js] " + msg)
	default:
		w.log.Debug("[js] " + msg)
	}
}

// New 创建并启动一个 Worker，执行 prelude
func New(opts Options) (*Worker, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 64 << 20
	}
	w := &Worker{opts: opts, log: opts.Logger.With("worker", opts.Name)}
	registry := require.NewRegistry()
	registry.RegisterNativeModule(console.ModuleName, console.RequireWithPrinter(consolePrinter{w}))
	registry.RegisterNativeModule(buffer.ModuleName, buffer.Require)
	registry.RegisterNativeModule(url.ModuleName, url.Require)
	registry.RegisterNativeModule(process.ModuleName, process.Require)
	w.loop = eventloop.NewEventLoop(eventloop.WithRegistry(registry), eventloop.EnableConsole(true))
	w.loop.Start()

	err := w.runSync(context.Background(), func(vm *goja.Runtime) error {
		w.vmRef.Store(vm)
		vm.SetMaxCallStackSize(4096)
		buffer.Enable(vm)
		url.Enable(vm)
		process.Enable(vm)
		installHost(w, vm)
		if _, err := vm.RunString(`
var __await = function(v, cb) {
  Promise.resolve(v).then(function(r) {
    var s
    try { s = r === undefined ? 'null' : JSON.stringify(r); if (s === undefined) s = 'null' } catch (e) { return cb(String(e && e.message || e), null) }
    cb(null, s)
  }, function(e) {
    var msg = e && e.message ? String(e.message) : String(e)
    if (e && e.stack && String(e.stack).indexOf(msg) === -1) msg += ' @ ' + String(e.stack).split('\n').slice(0, 3).join(' | ')
    else if (e && e.stack) msg = String(e.stack).split('\n').slice(0, 4).join(' | ')
    cb(msg, null)
  })
}`); err != nil {
			return err
		}
		aw, ok := goja.AssertFunction(vm.Get("__await"))
		if !ok {
			return errors.New("__await not callable")
		}
		w.await = aw
		jp, ok := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
		if !ok {
			return errors.New("JSON.parse not callable")
		}
		w.jsonParse = jp
		if opts.Prelude != "" {
			if _, err := vm.RunScript("prelude.js", opts.Prelude); err != nil {
				return fmt.Errorf("prelude: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		w.Stop()
		return nil, err
	}
	return w, nil
}

// Stop 终止事件循环
func (w *Worker) Stop() {
	if w.stopped.Swap(true) {
		return
	}
	w.loop.Terminate()
}

// run 在事件循环内执行 fn（不等待）
func (w *Worker) run(fn func(vm *goja.Runtime)) bool {
	if w.stopped.Load() {
		return false
	}
	return w.loop.RunOnLoop(func(vm *goja.Runtime) {
		w.running.Store(true)
		defer w.running.Store(false)
		fn(vm)
	})
}

// runSync 在事件循环内执行 fn 并等待其返回
func (w *Worker) runSync(ctx context.Context, fn func(vm *goja.Runtime) error) error {
	done := make(chan error, 1)
	ok := w.run(func(vm *goja.Runtime) {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("js panic: %v", r)
			}
		}()
		done <- fn(vm)
	})
	if !ok {
		return errors.New("worker stopped")
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		w.interruptIfBusy()
		return ctx.Err()
	}
}

// RunScript 执行一段脚本（例如音源脚本本体）
func (w *Worker) RunScript(ctx context.Context, name, src string) error {
	return w.runSync(ctx, func(vm *goja.Runtime) error {
		_, err := vm.RunScript(name, src)
		return err
	})
}

// Eval 执行表达式并返回 JSON 化结果（支持 Promise）
func (w *Worker) Eval(ctx context.Context, expr string) (json.RawMessage, error) {
	return w.callInternal(ctx, func(vm *goja.Runtime) (goja.Value, error) {
		return vm.RunString(expr)
	})
}

// CallJSON 调用全局函数 fnName，参数以 JSON 传入，返回值以 JSON 返回；等待 Promise 完成
func (w *Worker) CallJSON(ctx context.Context, fnName string, args ...any) (json.RawMessage, error) {
	encoded := make([]string, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return nil, err
		}
		encoded[i] = string(b)
	}
	return w.callInternal(ctx, func(vm *goja.Runtime) (goja.Value, error) {
		fnv := vm.Get(fnName)
		fn, ok := goja.AssertFunction(fnv)
		if !ok {
			return nil, fmt.Errorf("function %s not found", fnName)
		}
		vals := make([]goja.Value, len(encoded))
		for i, s := range encoded {
			v, err := w.jsonParse(goja.Undefined(), vm.ToValue(s))
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		return fn(goja.Undefined(), vals...)
	})
}

type jsResult struct {
	data json.RawMessage
	err  error
}

func (w *Worker) callInternal(ctx context.Context, produce func(vm *goja.Runtime) (goja.Value, error)) (json.RawMessage, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
	}
	res := make(chan jsResult, 1)
	var finished atomic.Bool
	deliver := func(r jsResult) {
		if finished.CompareAndSwap(false, true) {
			res <- r
		}
	}
	ok := w.run(func(vm *goja.Runtime) {
		defer func() {
			if r := recover(); r != nil {
				deliver(jsResult{err: fmt.Errorf("js panic: %v", r)})
			}
		}()
		v, err := produce(vm)
		if err != nil {
			deliver(jsResult{err: normalizeJSError(err)})
			return
		}
		cb := vm.ToValue(func(call goja.FunctionCall) goja.Value {
			if e := call.Argument(0); !goja.IsNull(e) && !goja.IsUndefined(e) {
				deliver(jsResult{err: errors.New(e.String())})
				return goja.Undefined()
			}
			deliver(jsResult{data: json.RawMessage(call.Argument(1).String())})
			return goja.Undefined()
		})
		if _, err := w.await(goja.Undefined(), v, cb); err != nil {
			deliver(jsResult{err: normalizeJSError(err)})
		}
	})
	if !ok {
		return nil, errors.New("worker stopped")
	}
	select {
	case r := <-res:
		return r.data, r.err
	case <-ctx.Done():
		finished.Store(true)
		w.interruptIfBusy()
		return nil, fmt.Errorf("js call timeout: %w", ctx.Err())
	}
}

// interruptIfBusy 若 JS 正在长时间执行（死循环等）则中断它并清除标记
func (w *Worker) interruptIfBusy() {
	if !w.running.Load() {
		return
	}
	vm := w.loopVM()
	if vm == nil {
		return
	}
	vm.Interrupt("timeout")
	w.loop.RunOnLoop(func(vm *goja.Runtime) { vm.ClearInterrupt() })
}

// loopVM 通过一次空 RunOnLoop 无法在外部拿到 vm，这里借助初始化时保存的引用
func (w *Worker) loopVM() *goja.Runtime { return w.vmRef.Load() }

func normalizeJSError(err error) error {
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return errors.New(ex.Value().String())
	}
	var ie *goja.InterruptedError
	if errors.As(err, &ie) {
		return errors.New("js interrupted: " + fmt.Sprint(ie.Value()))
	}
	return err
}
