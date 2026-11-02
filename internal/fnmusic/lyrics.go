package fnmusic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

type lyricItem struct {
	GUID    string `json:"guid"`
	Content string `json:"content"`
	IsLRC   bool   `json:"isLRC"`
	Offset  int    `json:"offset"`
}

type lyricData struct {
	List      []lyricItem `json:"list"`
	Preferred string      `json:"preferred"`
}

func (s *Server) lyrics(w http.ResponseWriter, r *http.Request) {
	event, finish := s.beginResourceDiagnostic(w, r, "getLyricsBySongId", "json")
	defer finish()
	w, finishResponse := diagnostics.CaptureResponse(w, r, event)
	defer finishResponse()
	track, err := s.allowedTrack(r, r.URL.Query().Get("trackGUID"))
	if resourceError(w, err) {
		return
	}
	event.TrackID = track.TrackID()
	_, event.Cached = s.Catalog.CachedLyric(track)
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	lyrics, err := s.loadLyric(ctx, currentUser(r).ID, track)
	if r.Context().Err() != nil {
		return
	}
	if errors.Is(err, admission.ErrBusy) {
		event.Error = "busy"
		w.Header().Set("Retry-After", "2")
		failure(w, http.StatusServiceUnavailable, "歌词服务繁忙")
		return
	}
	data := lyricData{List: []lyricItem{}}
	count, lines, synced := 0, 0, false
	event.Result = "empty"
	if err == nil && lyrics != nil {
		content := lyricContent(lyrics)
		if strings.TrimSpace(content) != "" {
			guid := identifier("lyric", track.TrackID())
			data.List = append(data.List, lyricItem{GUID: guid, Content: content, IsLRC: lyrics.Synced, Offset: lyrics.Offset})
			data.Preferred = guid
			count, lines, synced = 1, len(lyrics.Lines), lyrics.Synced
			event.Result = "ok"
		}
	} else {
		event.Error = diagnostics.ErrorCode(err)
	}
	event.Count, event.Lines, event.Synced = &count, &lines, &synced
	success(w, data)
}

func (s *Server) loadLyric(ctx context.Context, userID int64, track *music.Info) (*music.Lyrics, error) {
	release, err := s.Catalog.AcquireRequest(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer release()
	return s.Catalog.Lyric(ctx, track)
}

func lyricContent(lyrics *music.Lyrics) string {
	if !lyrics.Synced {
		return lyrics.PlainText()
	}
	// Work on a copy: cached lyrics are shared. Offset is sent only in the protocol field.
	lines := make([]music.Line, 0, len(lyrics.Lines)+len(lyrics.Trans)+len(lyrics.Roma))
	lines = append(lines, lyrics.Lines...)
	lines = append(lines, lyrics.Trans...)
	lines = append(lines, lyrics.Roma...)
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Start < lines[j].Start })
	var content strings.Builder
	for _, line := range lines {
		start := max(0, line.Start)
		fmt.Fprintf(&content, "[%02d:%02d.%03d]%s\n", start/60000, start%60000/1000, start%1000, line.Value)
	}
	return strings.TrimSuffix(content.String(), "\n")
}
