package fnmusic

import (
	"crypto/rand"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/mediastream"
)

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	event := &diagnostics.Event{RequestID: "req-" + rand.Text(), Stage: "client_response", Endpoint: "stream", Client: "fnmusic", Method: r.Method, Format: "binary", Result: "failed", Error: "none"}
	started := time.Now()
	if s.Media != nil {
		defer func() {
			event.ElapsedMS = time.Since(started).Milliseconds()
			if r.Context().Err() != nil {
				event.Result, event.Error = "failed", diagnostics.ErrorCode(r.Context().Err())
			}
			s.Media.Diagnostics.Add(*event)
		}()
	}
	guid := r.URL.Query().Get("guid")
	if guid == "" {
		guid = chi.URLParam(r, "guid")
	}
	track, err := s.allowedTrack(r, guid)
	if resourceError(w, err) {
		event.Status = http.StatusNotFound
		if !errors.Is(err, db.ErrNotFound) {
			event.Status = http.StatusInternalServerError
		}
		return
	}
	if s.Media == nil {
		failure(w, http.StatusServiceUnavailable, "音乐交付未配置")
		return
	}
	event.TrackID = track.TrackID()
	r = mediastream.Bind(r, currentUser(r), event)
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.Media.ServeTrack(w, r, currentUser(r), track.TrackID(), "", writeMediaError)
}

func writeMediaError(w http.ResponseWriter, r *http.Request, code int, message string) {
	status := http.StatusBadGateway
	switch code {
	case mediastream.ErrBusy:
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "2")
	case mediastream.ErrWrongAuth:
		status = http.StatusUnauthorized
	case mediastream.ErrMissingParam:
		status = http.StatusBadRequest
	case mediastream.ErrNotFound:
		status = http.StatusNotFound
	}
	mediastream.RecordResponse(r, status)
	failure(w, status, message)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []struct {
			Type    string `json:"eventType"`
			Payload struct {
				GUID string `json:"trackGUID"`
			} `json:"payload"`
		} `json:"events"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len(body.Events) > 100 {
		failure(w, http.StatusBadRequest, "事件数量超限")
		return
	}
	for _, event := range body.Events {
		if event.Type != "track_play" {
			continue
		}
		track, err := s.allowedTrack(r, event.Payload.GUID)
		if resourceError(w, err) {
			return
		}
		// 正在播放不等于完整收听，不在此累计听歌时长。
		if err := s.DB.SetNowPlaying(r.Context(), currentUser(r).ID, "fnmusic", track.TrackID()); err != nil {
			resourceError(w, err)
			return
		}
	}
	success(w, map[string]any{})
}
