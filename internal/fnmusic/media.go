package fnmusic

import (
	"bytes"
	"crypto/rand"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"sync"
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

var placeholderOnce sync.Once
var placeholder []byte

func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("coverId")
	kind, guid, ok := strings.Cut(id, "_")
	if !ok || !validGUID(guid) {
		failure(w, http.StatusNotFound, "封面不存在")
		return
	}
	allowed := false
	switch kind {
	case "track":
		_, err := s.allowedTrack(r, guid)
		allowed = err == nil
	case "playlist":
		_, err := s.allowedPlaylist(r, guid)
		allowed = err == nil
	case "album", "artist":
		id, err := s.resolve(r.Context(), kind, guid)
		if err == nil {
			tracks, err := s.visibleTracks(r)
			if err == nil {
				for _, track := range tracks {
					if (kind == "album" && track.AlbumSubID() == id) || (kind == "artist" && track.ArtistSubID() == id) {
						allowed = true
						break
					}
				}
			}
		}
	}
	if !allowed {
		failure(w, http.StatusNotFound, "封面不存在或无权访问")
		return
	}
	placeholderOnce.Do(func() {
		art := image.NewRGBA(image.Rect(0, 0, 128, 128))
		for y := 0; y < 128; y++ {
			for x := 0; x < 128; x++ {
				d := (x-64)*(x-64) + (y-64)*(y-64)
				c := color.RGBA{29, 43, 48, 255}
				if d < 48*48 {
					c = color.RGBA{62, 86, 89, 255}
				}
				if d < 12*12 {
					c = color.RGBA{217, 158, 91, 255}
				}
				art.SetRGBA(x, y, c)
			}
		}
		var buffer bytes.Buffer
		_ = png.Encode(&buffer, art)
		placeholder = buffer.Bytes()
	})
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, no-store")
	_, _ = w.Write(placeholder)
}

func (s *Server) lyrics(w http.ResponseWriter, r *http.Request) {
	_, err := s.allowedTrack(r, r.URL.Query().Get("trackGUID"))
	if resourceError(w, err) {
		return
	}
	success(w, map[string]any{"list": []any{}, "preferred": ""})
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
