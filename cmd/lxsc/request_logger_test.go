package main

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type loggerFlushWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *loggerFlushWriter) FlushError() error { return w.err }

func TestRequestLoggerPreservesFlushErrors(t *testing.T) {
	want := errors.New("test connection reset")
	w := &loggerFlushWriter{ResponseRecorder: httptest.NewRecorder(), err: want}
	var got error
	handler := requestLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "response")
		got = http.NewResponseController(w).Flush()
	}))
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/rest/getPlaylist", nil))
	if !errors.Is(got, want) {
		t.Fatalf("日志包装器吞掉了网络刷新错误: got=%v want=%v", got, want)
	}
}

type loggerCapabilityWriter struct{ *httptest.ResponseRecorder }

func (w *loggerCapabilityWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}
func (w *loggerCapabilityWriter) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(w.ResponseRecorder, r)
}
func (w *loggerCapabilityWriter) Push(string, *http.PushOptions) error {
	return http.ErrNotSupported
}

func TestRequestLoggerPreservesConnectionCapabilities(t *testing.T) {
	for _, major := range []int{1, 2} {
		writer := &loggerCapabilityWriter{httptest.NewRecorder()}
		handler := requestLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, hijack := w.(http.Hijacker)
			_, readFrom := w.(io.ReaderFrom)
			_, push := w.(http.Pusher)
			if hijack != (major == 1) || readFrom != (major == 1) || push != (major == 2) {
				t.Errorf("HTTP/%d 的连接能力发生变化: hijack=%v readFrom=%v push=%v", major, hijack, readFrom, push)
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("刷新失败: %v", err)
			}
		}))
		req := httptest.NewRequest(http.MethodGet, "/rest/getPlaylist", nil)
		req.ProtoMajor = major
		handler.ServeHTTP(writer, req)
		if writer.Code != http.StatusOK || !writer.Flushed {
			t.Fatal("只有刷新而没有正文的响应未正确发出")
		}
	}
}
