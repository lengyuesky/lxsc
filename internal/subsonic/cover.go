package subsonic

import (
	"context"
	"net/http"

	"lxsc/internal/coverart"
	"lxsc/internal/diagnostics"
)

const coverRequestTimeout = coverart.RequestTimeout

func (s *Server) initCoverClient() {
	s.coverOnce.Do(func() {
		if s.Covers == nil {
			s.Covers = &coverart.Service{}
		}
	})
}

func (s *Server) coverWithFallback(w http.ResponseWriter, r *http.Request, address, template string, fields map[string]string) {
	s.serveCover(r.Context(), w, r, address, template, fields)
}

func (s *Server) serveCover(ctx context.Context, w http.ResponseWriter, r *http.Request, address, template string, fields map[string]string) {
	s.initCoverClient()
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	s.Covers.Serve(w, r.WithContext(ctx), coverart.Resource{UserID: currentUser(r).ID, Address: address, Template: template, Fields: fields}, event)
}

func recordCoverOutcome(r *http.Request, origin string, cached bool, err error) {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	coverart.RecordOutcome(event, origin, cached, err)
}

func captureCoverResponse(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	if event != nil {
		event.Format = "binary"
	}
	return diagnostics.CaptureResponse(w, r, event)
}

// CoverPerformance reports the same shared service used by the other music protocol.
func (s *Server) CoverPerformance() any {
	s.initCoverClient()
	return s.Covers.Performance()
}
