// Package logbuf 提供内存环形日志，供管理页查看
package logbuf

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Entry 一条日志
type Entry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Buffer 环形缓冲
type Buffer struct {
	mu   sync.Mutex
	buf  []Entry
	size int
}

// New 创建
func New(size int) *Buffer { return &Buffer{size: size} }

// Add 追加
func (b *Buffer) Add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, e)
	if len(b.buf) > b.size {
		b.buf = b.buf[len(b.buf)-b.size:]
	}
}

// List 最近 n 条
func (b *Buffer) List(n int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.buf) {
		n = len(b.buf)
	}
	out := make([]Entry, n)
	copy(out, b.buf[len(b.buf)-n:])
	return out
}

// Handler slog Handler：同时写入下游 handler 与环形缓冲
type Handler struct {
	next  slog.Handler
	buf   *Buffer
	attrs []slog.Attr
}

// NewHandler 创建
func NewHandler(next slog.Handler, buf *Buffer) *Handler { return &Handler{next: next, buf: buf} }

// Enabled 实现 slog.Handler
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

// Handle 实现 slog.Handler
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	msg := r.Message
	for _, a := range h.attrs {
		msg += " " + a.Key + "=" + a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		msg += " " + a.Key + "=" + a.Value.String()
		return true
	})
	h.buf.Add(Entry{Time: r.Time.Format(time.DateTime), Level: r.Level.String(), Message: msg})
	return h.next.Handle(ctx, r)
}

// WithAttrs 实现 slog.Handler
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs), buf: h.buf, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

// WithGroup 实现 slog.Handler
func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), buf: h.buf, attrs: h.attrs}
}
