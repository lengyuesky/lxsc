package fnmusic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lxsc/internal/coverart"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

func (s *Server) coverID(kind, guid string, track *music.Info) string {
	parts := []string{}
	if track != nil {
		parts = append(parts, track.TrackID(), track.Name(), track.Singer(), track.Album(), track.Img(), strconv.Itoa(track.Duration()))
	}
	return s.coverVersionID(kind, guid, parts...)
}

func (s *Server) coverVersionID(kind, guid string, metadata ...string) string {
	parts := []string{"v1", kind, guid, s.GUID, s.Settings.Get().CustomCoverURL}
	parts = append(parts, metadata...)
	// Version presentation IDs, not resource GUIDs: old client-side placeholders must refresh.
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return kind + "_" + guid + "_v1-" + hex.EncodeToString(sum[:6])
}

func parseCoverID(id string) (kind, guid string, valid bool) {
	kind, tail, ok := strings.Cut(id, "_")
	if !ok {
		return "", "", false
	}
	guid, revision, versioned := strings.Cut(tail, "_")
	if versioned {
		if len(revision) != 15 || !strings.HasPrefix(revision, "v1-") || strings.ToLower(revision) != revision {
			return "", "", false
		}
		if _, err := hex.DecodeString(revision[3:]); err != nil {
			return "", "", false
		}
	}
	return kind, guid, validGUID(guid)
}

func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	event, finish := s.beginResourceDiagnostic(w, r, "getCoverArt", "binary")
	defer finish()
	w, finishResponse := diagnostics.CaptureResponse(w, r, event)
	defer finishResponse()
	if s.uploadedCover(w, r, event) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), coverart.RequestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	resource, track, err := s.coverResource(r)
	if resourceError(w, err) {
		return
	}
	if s.Covers == nil {
		failure(w, http.StatusServiceUnavailable, "封面交付未配置")
		return
	}
	if track != nil {
		event.TrackID = track.TrackID()
		if resource.Address == "" {
			// URL resolution must leave time for validated image delivery and fallback.
			coverCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			resource.Address = s.Catalog.Cover(coverCtx, track)
			stop()
		}
		resource.Template = s.Settings.Get().CustomCoverURL
	}
	if strings.HasPrefix(resource.Address, "http://") {
		resource.Address = "https://" + strings.TrimPrefix(resource.Address, "http://")
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.Covers.Serve(w, r, resource, event)
}

func (s *Server) coverResource(r *http.Request) (coverart.Resource, *music.Info, error) {
	resource := coverart.Resource{UserID: currentUser(r).ID}
	kind, guid, ok := parseCoverID(r.URL.Query().Get("coverId"))
	if !ok {
		return resource, nil, db.ErrNotFound
	}
	if granted, found := s.searches.get(r.Context(), kind, guid, time.Now()); found && (kind == "album" || kind == "artist") {
		resource.Template = s.Settings.Get().CustomCoverURL
		if kind == "album" && granted.album != nil {
			meta := granted.album
			resource.Address, resource.Fields = meta.Image, map[string]string{"album": meta.Name, "artist": meta.Artist, "source": meta.Source, "id": meta.ID}
			return resource, nil, nil
		}
		if kind == "artist" && granted.artist != nil {
			ref := granted.artist
			resource.Address, resource.Fields = ref.Avatar, map[string]string{"artist": ref.Name, "source": ref.Source, "id": ref.ID}
			return resource, nil, nil
		}
	}
	var track *music.Info
	switch kind {
	case "track":
		var err error
		track, err = s.allowedTrack(r, guid)
		if err != nil {
			return resource, nil, err
		}
	case "playlist":
		playlist, err := s.allowedPlaylist(r, guid)
		if err != nil {
			return resource, nil, err
		}
		for _, id := range playlist.TrackIDs {
			track, err = s.Catalog.LocalTrack(r.Context(), id)
			if err == nil {
				break
			}
			if !errors.Is(err, db.ErrNotFound) {
				return resource, nil, err
			}
		}
		if track == nil {
			// An authorized empty playlist has no song metadata to send to a fallback API.
			resource.Fields = map[string]string{}
			return resource, nil, nil
		}
	case "album", "artist":
		id, err := s.resolve(r.Context(), kind, guid)
		if err != nil {
			return resource, nil, err
		}
		tracks, err := s.visibleTracks(r)
		if err != nil {
			return resource, nil, err
		}
		for _, candidate := range tracks {
			if (kind == "album" && candidate.AlbumSubID() == id) || (kind == "artist" && candidate.ArtistSubID() == id) {
				track = candidate
				break
			}
		}
		if track == nil {
			return resource, nil, db.ErrNotFound
		}
	default:
		return resource, nil, db.ErrNotFound
	}
	resource.Fields = track.TemplateValues()
	if kind == "album" || kind == "artist" {
		delete(resource.Fields, "title")
	}
	if kind == "artist" {
		delete(resource.Fields, "album")
	}
	return resource, track, nil
}
