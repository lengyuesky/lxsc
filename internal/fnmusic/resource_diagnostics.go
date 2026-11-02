package fnmusic

import (
	"crypto/rand"
	"net/http"
	"time"

	"lxsc/internal/diagnostics"
)

func (s *Server) beginResourceDiagnostic(w http.ResponseWriter, r *http.Request, endpoint, format string) (*diagnostics.Event, func()) {
	events := s.Diagnostics
	if events == nil && s.Media != nil {
		events = s.Media.Diagnostics
	}
	event := &diagnostics.Event{RequestID: "req-" + rand.Text(), Stage: "client_response", Endpoint: endpoint, Client: "fnmusic", Method: r.Method, Format: format, Result: "unavailable", Error: "none"}
	w.Header().Set("X-Request-ID", event.RequestID)
	started := time.Now()
	return event, func() {
		event.ElapsedMS = time.Since(started).Milliseconds()
		if r.Context().Err() != nil {
			if event.Error == "none" {
				event.Error = diagnostics.ErrorCode(r.Context().Err())
			}
			event.Result = "failed"
		}
		events.Add(*event)
	}
}
