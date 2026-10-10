package subsonic

import (
	"errors"
	"net/http"
	"strconv"

	"lxsc/internal/admission"
	"lxsc/internal/diagnostics"
)

func recordCoverOutcome(r *http.Request, origin string, cached bool, err error) {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	if event == nil {
		return
	}
	event.CoverOrigin, event.Cached = origin, cached
	event.Error = diagnostics.ErrorCode(err)
	if errors.Is(err, admission.ErrBusy) {
		event.Error = "busy"
	}
	if origin == "placeholder" || origin == "stale" {
		event.Result = "unavailable"
	}
}

type coverResponseWriter struct {
	http.ResponseWriter
	status            int
	written, expected int64
	err               error
	flushed           bool
}

func (w *coverResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *coverResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.flushed = false
	w.ResponseWriter.WriteHeader(status)
}
func (w *coverResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.expected += int64(len(data))
	w.flushed = false
	n, err := w.ResponseWriter.Write(data)
	w.written += int64(n)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *coverResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	w.flushed = true
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.err = err
	}
	return err
}

func (w *coverResponseWriter) Flush() { _ = w.FlushError() }

// 覆盖图片、302、304 和错误响应，并在最终刷新后记录真实写出结果。
func captureCoverResponse(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	if event == nil {
		return w, func() {}
	}
	capture := &coverResponseWriter{ResponseWriter: w}
	return capture, func() {
		event.Status, event.Format = capture.status, "binary"
		if event.Error == "" {
			event.Error = "none"
		}
		if capture.status == 0 || capture.status >= 400 {
			event.Result = "failed"
		} else if event.CoverOrigin != "placeholder" && event.CoverOrigin != "stale" {
			event.Result = "ok"
		}
		expected := capture.expected
		if declared, err := strconv.ParseInt(capture.Header().Get("Content-Length"), 10, 64); err == nil && declared >= 0 {
			expected = declared
		}
		if r.Method == http.MethodHead || capture.status == http.StatusNotModified {
			expected = 0
		}
		issue := ""
		if capture.err != nil || capture.written != expected {
			issue = "write_error"
		}
		if capture.status != 0 && !capture.flushed {
			if err := http.NewResponseController(capture).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				issue = "write_error"
			}
		}
		recordClientDelivery(r, int(capture.written), int(expected), issue)
	}
}
