package fnmusic

import (
	"crypto/rand"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/mediastream"
	"lxsc/internal/playback"
)

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	s.deliverTrack(w, r, false)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	s.deliverTrack(w, r, true)
}

func (s *Server) deliverTrack(w http.ResponseWriter, r *http.Request, download bool) {
	endpoint := "stream"
	if download {
		endpoint = "download"
	}
	event := &diagnostics.Event{RequestID: "req-" + rand.Text(), Stage: "client_response", Endpoint: endpoint, Client: "fnmusic", Method: r.Method, Format: "binary", Result: "failed", Error: "none"}
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
	track, transient, err := s.trackAccess(r, guid)
	if err != nil {
		event.Status = http.StatusNotFound
		if !errors.Is(err, db.ErrNotFound) {
			event.Status = http.StatusInternalServerError
		}
		if download {
			downloadFailure(w, r, event.Status)
		} else {
			resourceError(w, err)
		}
		return
	}
	id := track.TrackID()
	if !transient {
		id, err = s.resolve(r.Context(), "track", guid)
		if err != nil {
			if download {
				downloadFailure(w, r, http.StatusNotFound)
			} else {
				resourceError(w, err)
			}
			return
		}
	}
	if s.Media == nil {
		if download {
			downloadFailure(w, r, http.StatusServiceUnavailable)
			return
		}
		failure(w, http.StatusServiceUnavailable, "音乐交付未配置")
		return
	}
	event.TrackID = id
	if !download {
		session, _ := r.Context().Value(playbackSessionKey{}).(string)
		r = playback.Bind(r, playback.Client{Protocol: "fnmusic", Name: "飞牛音乐", Session: session})
	}
	r = mediastream.Bind(r, currentUser(r), event)
	w.Header().Set("Referrer-Policy", "no-referrer")
	if download {
		resume := nonzeroDownloadRange(r)
		baseline, known := s.downloads.get(r.Context(), guid)
		if resume {
			if !known || baseline.ambiguous || baseline.value == "" || (r.Header.Get("If-Range") != "" && r.Header.Get("If-Range") != baseline.value) {
				event.Error = "invalid_response"
				downloadFailure(w, r, http.StatusRequestedRangeNotSatisfiable)
				return
			}
			r = r.Clone(r.Context())
			r.Header.Set("If-Range", baseline.value)
		}
		if !safeDownloadRange(r) {
			event.Error = "invalid_response"
			downloadFailure(w, r, http.StatusRequestedRangeNotSatisfiable)
			return
		}
		r = mediastream.ObserveMediaDescription(r, func(description mediastream.MediaDescription) error {
			if r.Method == http.MethodHead {
				return nil
			}
			if resume {
				if !s.downloads.matches(r.Context(), guid, baseline, description) {
					return db.ErrNotFound
				}
			} else {
				s.downloads.remember(r.Context(), guid, description)
			}
			return nil
		})
		writer := &downloadWriter{ResponseWriter: w, title: track.Name()}
		s.Media.ServeDownloadTrack(writer, r, currentUser(r), id, writeDownloadError)
		return
	}
	s.Media.ServeTrack(w, r, currentUser(r), id, "", writeMediaError)
}

func nonzeroDownloadRange(r *http.Request) bool {
	value := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	first, _, _ := strings.Cut(value, "-")
	return first != "" && first != "0"
}

// 没有实体校验条件的非零续传可能拼接不同音源/音质，必须重新下载整份文件。
func safeDownloadRange(r *http.Request) bool {
	value := r.Header.Get("Range")
	if value == "" {
		return true
	}
	value, ok := strings.CutPrefix(value, "bytes=")
	if !ok || strings.Contains(value, ",") {
		return false
	}
	start, _, ok := strings.Cut(value, "-")
	if !ok {
		return false
	}
	if start == "0" {
		return true
	}
	if start == "" {
		return false
	}
	if offset, err := strconv.ParseInt(start, 10, 64); err != nil || offset < 0 {
		return false
	}
	return r.Header.Get("If-Range") != ""
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
		// 只按开始事件估算，媒体 GET、Range、HEAD 与下载不增加收听次数。
		session, _ := r.Context().Value(playbackSessionKey{}).(string)
		if err := s.Playback.FnStart(r.Context(), currentUser(r), session, db.PlaybackTrack{ID: track.TrackID(), Name: track.Name(), Singer: track.Singer(), Platform: track.Source()}, int64(track.Duration())*1000); err != nil {
			failure(w, http.StatusInternalServerError, "保存播放统计失败")
			return
		}
		if err := s.DB.SetNowPlaying(r.Context(), currentUser(r).ID, "fnmusic", track.TrackID()); err != nil {
			resourceError(w, err)
			return
		}
	}
	success(w, map[string]any{})
}
