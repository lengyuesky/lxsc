package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/music"
)

// searchStream 按行发送 JSON，每个平台完成后立即刷新响应。
func (s *Server) searchStream(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readSearch(w, r)
	if !ok {
		return
	}
	release, ok := s.admitSearch(w, r)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	hardDeadline := time.Now().Add(17 * time.Second)
	// 每次写入最多三秒，同时限制整个流；保留截止时间覆盖 HTTP 协议收尾。
	done := make(chan struct{})
	stop := context.AfterFunc(r.Context(), func() { _ = controller.SetWriteDeadline(time.Now()); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	encoder := json.NewEncoder(w)
	writeFailed := false
	write := func(value any) bool {
		if writeFailed || r.Context().Err() != nil {
			return false
		}
		deadline := time.Now().Add(3 * time.Second)
		if deadline.After(hardDeadline) {
			deadline = hardDeadline
		}
		if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
			writeFailed = true
			cancel()
			return false
		}
		if encoder.Encode(value) != nil || controller.Flush() != nil {
			writeFailed = true
			cancel()
			return false
		}
		// 等待下个平台时恢复整体期限，避免 HTTP/2 的写计时器误伤慢搜索。
		if r.Context().Err() == nil {
			_ = controller.SetWriteDeadline(hardDeadline)
		}
		return true
	}
	if !write(map[string]any{"type": "start", "sources": body.Sources}) {
		return
	}
	s.Catalog.SearchProgress(ctx, body.Query, music.SearchOptions{Sources: body.Sources, Page: body.Page, Limit: 20}, func(result music.SearchPlatformResult) {
		if r.Context().Err() != nil {
			return
		}
		status, message := "ok", ""
		if result.Err != nil {
			status, message = "error", "平台搜索失败，请稍后重试"
			if errors.Is(result.Err, context.DeadlineExceeded) {
				status, message = "timeout", "平台搜索超时"
			}
			if errors.Is(result.Err, admission.ErrBusy) {
				status, message = "busy", admission.ErrBusy.Error()
			}
		}
		tracks := make([]trackView, 0, len(result.Tracks))
		for _, in := range result.Tracks {
			tracks = append(tracks, infoTrackView(in))
		}
		write(map[string]any{"type": "platform", "source": result.Source, "status": status, "error": message, "tracks": tracks, "cached": result.Cached, "elapsedMs": result.Duration.Milliseconds()})
	})
	if r.Context().Err() == nil {
		write(map[string]string{"type": "done"})
	}
}

func (s *Server) admitSearch(w http.ResponseWriter, r *http.Request) (func(), bool) {
	release, err := s.Catalog.AcquireRequest(r.Context(), currentUser(r).ID)
	if err != nil {
		if r.Context().Err() == nil {
			w.Header().Set("Retry-After", "2")
			fail(w, 503, admission.ErrBusy.Error())
		}
		return nil, false
	}
	return release, true
}
