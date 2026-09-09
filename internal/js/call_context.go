package js

import (
	"context"

	"github.com/dop251/goja"
)

// callScope 只归属一次 Go→JS 调用，不按 Worker 取消其他调用或初始化后台任务。
// 以下上下文切换只在事件循环内进行；activeCall 供外部取消时核对正在执行的归属。
type callScope struct{ ctx context.Context }

type callTracker struct {
	worker   *Worker
	current  *callScope
	previous *callScope
}

func (t *callTracker) Grab() any { return t.current }
func (t *callTracker) Resumed(value any) {
	t.previous = t.current
	t.set(value.(*callScope))
}
func (t *callTracker) Exited() { t.set(t.previous) }
func (t *callTracker) set(scope *callScope) {
	w := t.worker
	w.callMu.Lock()
	defer w.callMu.Unlock()
	if w.callInterrupted {
		w.loopVM().ClearInterrupt()
		w.callInterrupted = false
	}
	t.current = scope
	w.activeCall.Store(scope)
}
func (w *Worker) enterCall(scope *callScope) func() {
	previous := w.calls.current
	w.calls.set(scope)
	return func() { w.calls.set(previous) }
}

// Promise/async-await 使用 goja 的上下文跟踪；定时器和原生 HTTP 回调显式传递归属。
func (w *Worker) installCallContext(vm *goja.Runtime) {
	w.calls.worker = w
	vm.SetAsyncContextTracker(&w.calls)
	for _, name := range []string{"Timeout", "Interval", "Immediate"} {
		schedule, _ := goja.AssertFunction(vm.Get("set" + name))
		clear, _ := goja.AssertFunction(vm.Get("clear" + name))
		_ = vm.Set("set"+name, func(call goja.FunctionCall) goja.Value {
			scope := w.calls.current
			callback, ok := goja.AssertFunction(call.Argument(0))
			if scope == nil || !ok {
				value, err := schedule(call.This, call.Arguments...)
				if err != nil {
					panic(err)
				}
				return value
			}
			if scope.ctx.Err() != nil {
				return goja.Undefined()
			}
			args := append([]goja.Value(nil), call.Arguments...)
			var stop func() bool
			args[0] = vm.ToValue(func(callbackCall goja.FunctionCall) goja.Value {
				if name != "Interval" {
					stop()
				}
				if scope.ctx.Err() != nil {
					return goja.Undefined()
				}
				defer w.enterCall(scope)()
				value, err := callback(callbackCall.This, callbackCall.Arguments...)
				if err != nil {
					panic(err)
				}
				return value
			})
			handle, err := schedule(call.This, args...)
			if err != nil {
				panic(err)
			}
			stop = context.AfterFunc(scope.ctx, func() {
				w.run(func(vm *goja.Runtime) { _, _ = clear(goja.Undefined(), handle) })
			})
			return handle
		})
	}
}

// 切换归属与中断在同一锁内，防止取消刚结束的调用误伤下一次执行。
func (w *Worker) interruptCall(scope *callScope) {
	w.callMu.Lock()
	defer w.callMu.Unlock()
	if w.activeCall.Load() == scope {
		w.callInterrupted = true
		w.loopVM().Interrupt("timeout")
	}
}
