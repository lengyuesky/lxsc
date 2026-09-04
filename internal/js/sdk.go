package js

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// SDKPool 运行 musicSdk bundle 的无状态工作池
type SDKPool struct {
	workers []*Worker
	next    atomic.Uint64
	log     *slog.Logger
}

// NewSDKPool 创建 n 个 SDK worker
func NewSDKPool(n int, prelude, bundle string, secure, insecure *http.Client, log *slog.Logger) (*SDKPool, error) {
	if n <= 0 {
		n = 1
	}
	p := &SDKPool{log: log}
	for i := 0; i < n; i++ {
		w, err := New(Options{Name: fmt.Sprintf("sdk-%d", i), Logger: log, HTTP: secure, HTTPInsecure: insecure, Prelude: prelude})
		if err != nil {
			p.Close()
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = w.RunScript(ctx, "sdk.bundle.js", bundle)
		cancel()
		if err != nil {
			w.Stop()
			p.Close()
			return nil, fmt.Errorf("加载 SDK bundle 失败: %w", err)
		}
		p.workers = append(p.workers, w)
	}
	return p, nil
}

// Close 停止全部 worker
func (p *SDKPool) Close() {
	for _, w := range p.workers {
		w.Stop()
	}
}

// Call 调用 SDK 方法，path 形如 "wy.musicSearch.search"，结果解码到 out
func (p *SDKPool) Call(ctx context.Context, path string, out any, args ...any) error {
	if len(p.workers) == 0 {
		return fmt.Errorf("sdk pool empty")
	}
	if args == nil {
		args = []any{}
	}
	w := p.workers[int(p.next.Add(1)-1)%len(p.workers)]
	raw, err := w.CallJSON(ctx, "__sdk_call", path, args)
	if err != nil {
		return fmt.Errorf("sdk %s: %w", path, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("sdk %s: 解析结果失败: %w", path, err)
	}
	return nil
}

// CallRaw 返回原始 JSON
func (p *SDKPool) CallRaw(ctx context.Context, path string, args ...any) (json.RawMessage, error) {
	if len(p.workers) == 0 {
		return nil, fmt.Errorf("sdk pool empty")
	}
	if args == nil {
		args = []any{}
	}
	w := p.workers[int(p.next.Add(1)-1)%len(p.workers)]
	raw, err := w.CallJSON(ctx, "__sdk_call", path, args)
	if err != nil {
		return nil, fmt.Errorf("sdk %s: %w", path, err)
	}
	return raw, nil
}

// Workers 返回 worker 列表（用于探测是否初始化）
func (p *SDKPool) Workers() []*Worker { return p.workers }

// CallFn 调用 bundle 中的任意全局函数
func (p *SDKPool) CallFn(ctx context.Context, fn string, args ...any) (json.RawMessage, error) {
	if len(p.workers) == 0 {
		return nil, fmt.Errorf("sdk pool empty")
	}
	w := p.workers[int(p.next.Add(1)-1)%len(p.workers)]
	return w.CallJSON(ctx, fn, args...)
}
