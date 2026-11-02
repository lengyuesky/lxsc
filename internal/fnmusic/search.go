package fnmusic

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lxsc/internal/admission"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

const searchTimeout = 8 * time.Second

var errSearchUnavailable = errors.New("search sources unavailable")

type searchCandidate struct {
	track    *music.Info
	album    *music.AlbumMeta
	artist   *music.ArtistRef
	playlist *db.Playlist
	local    bool
	count    int
}

func searchQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if !utf8.ValidString(query) || len(query) > 512 || strings.ContainsAny(query, "\x00\r\n") {
		failure(w, http.StatusBadRequest, "搜索关键词无效或过长")
		return "", false
	}
	return query, true
}

func matchesSearch(query string, values ...string) bool {
	query = strings.ToLower(query)
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func (s *Server) searchOptions() music.SearchOptions {
	settings := s.Settings.Get()
	return music.SearchOptions{Sources: settings.SearchSources, Page: 1, Limit: min(50, max(1, settings.SearchLimit))}
}

func (s *Server) remoteSearchTracks(ctx context.Context, query string, opts music.SearchOptions) ([]*music.Info, error) {
	lists := map[string][]*music.Info{}
	succeeded, busy := false, false
	s.Catalog.SearchProgress(ctx, query, opts, func(result music.SearchPlatformResult) {
		succeeded = succeeded || result.Err == nil
		busy = busy || errors.Is(result.Err, admission.ErrBusy)
		if result.Err == nil {
			for _, track := range result.Tracks {
				if track != nil && track.Source() == result.Source && diagnostics.ValidTrackID(track.TrackID()) {
					lists[result.Source] = append(lists[result.Source], track)
					if len(lists[result.Source]) >= opts.Limit {
						break
					}
				}
			}
		}
	})
	if !succeeded {
		if len(opts.Sources) == 0 {
			return nil, nil
		}
		if busy {
			return nil, admission.ErrBusy
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errSearchUnavailable
	}
	tracks := []*music.Info{}
	for index := 0; ; index++ {
		added := false
		for _, source := range opts.Sources {
			if list := lists[source]; index < len(list) {
				tracks = append(tracks, list[index])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return tracks, nil
}

func (s *Server) searchCandidates(r *http.Request, query, kind string) ([]searchCandidate, error) {
	items := []searchCandidate{}
	if query == "" {
		return items, nil
	}
	if kind == "playlist" {
		playlists, err := s.DB.ListPlaylists(r.Context(), currentUser(r).ID)
		if err != nil {
			return nil, err
		}
		for _, playlist := range playlists {
			if matchesSearch(query, playlist.Name, playlist.Comment) {
				items = append(items, searchCandidate{playlist: playlist, local: true})
			}
		}
		return items, nil
	}
	tracks, err := s.visibleTracks(r)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	for _, track := range tracks {
		id, matches := track.TrackID(), matchesSearch(query, track.Name(), track.Singer(), track.Album())
		if kind == "album" {
			id, matches = track.AlbumSubID(), matchesSearch(query, track.Album(), track.Singer())
		}
		if kind == "artist" {
			id, matches = track.ArtistSubID(), matchesSearch(query, track.PrimarySinger())
		}
		if !matches {
			continue
		}
		if index, found := seen[id]; found {
			items[index].count++
			continue
		}
		seen[id] = len(items)
		item := searchCandidate{track: track, local: true, count: 1}
		if kind == "album" {
			meta := albumFromTrack(track)
			item.album = &meta
		}
		if kind == "artist" {
			ref := artistFromTrack(track)
			item.artist = &ref
		}
		items = append(items, item)
	}
	opts := s.searchOptions()
	switch kind {
	case "track":
		remote, searchErr := s.remoteSearchTracks(r.Context(), query, opts)
		err = searchErr
		for _, track := range remote {
			if _, found := seen[track.TrackID()]; !found {
				seen[track.TrackID()] = len(items)
				items = append(items, searchCandidate{track: track})
			}
		}
	case "album":
		remote, _, searchErr := s.Catalog.SearchAlbumPage(r.Context(), query, opts)
		err = searchErr
		for _, meta := range remote {
			id := music.AlbumID(meta.Source, meta.ID, meta.Name, meta.Artist)
			if index, found := seen[id]; found {
				// An independently returned public entity can open its online catalogue.
				items[index] = searchCandidate{album: &meta, count: meta.SongCount}
			} else {
				seen[id] = len(items)
				items = append(items, searchCandidate{album: &meta, count: meta.SongCount})
			}
		}
	case "artist":
		remote, _, searchErr := s.Catalog.SearchArtistPage(r.Context(), query, opts)
		err = searchErr
		for _, ref := range remote {
			id := music.ArtistID(ref.Name)
			if index, found := seen[id]; found {
				items[index] = searchCandidate{artist: &ref}
			} else {
				seen[id] = len(items)
				items = append(items, searchCandidate{artist: &ref})
			}
		}
	}
	return items, err
}

func (s *Server) searchTracks(w http.ResponseWriter, r *http.Request)  { s.searchPage(w, r, "track") }
func (s *Server) searchAlbums(w http.ResponseWriter, r *http.Request)  { s.searchPage(w, r, "album") }
func (s *Server) searchArtists(w http.ResponseWriter, r *http.Request) { s.searchPage(w, r, "artist") }
func (s *Server) searchPlaylists(w http.ResponseWriter, r *http.Request) {
	s.searchPage(w, r, "playlist")
}

func (s *Server) searchPage(w http.ResponseWriter, r *http.Request, kind string) {
	event, finish := s.beginResourceDiagnostic(w, r, "search3", "json")
	defer finish()
	w, finishResponse := diagnostics.CaptureResponse(w, r, event)
	defer finishResponse()
	query, valid := searchQuery(w, r)
	if !valid {
		return
	}
	parent := r.Context()
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	if query != "" {
		release, err := s.Catalog.AcquireRequest(ctx, currentUser(r).ID)
		if searchFailure(w, event, err) {
			return
		}
		defer release()
	}
	items, err := s.searchCandidates(r, query, kind)
	if parent.Err() != nil {
		return
	}
	if len(items) == 0 && searchFailure(w, event, err) {
		return
	}
	if r.Context().Err() != nil && len(items) == 0 {
		return
	}
	r = r.WithContext(parent)
	start, end := pageBounds(r, len(items))
	views, viewErr := s.searchViews(r, items[start:end], kind)
	if resourceError(w, viewErr) {
		return
	}
	count := len(views)
	event.Count, event.Result = &count, "ok"
	if count == 0 {
		event.Result = "empty"
	}
	success(w, map[string]any{"list": views, "total": len(items), "sort": ""})
}

func searchFailure(w http.ResponseWriter, event *diagnostics.Event, err error) bool {
	if err == nil {
		return false
	}
	event.Result, event.Error = "failed", diagnostics.ErrorCode(err)
	status, message := http.StatusBadGateway, "搜索来源暂不可用，请稍后重试"
	if errors.Is(err, admission.ErrBusy) {
		status, message, event.Error = http.StatusServiceUnavailable, "搜索服务繁忙，请稍后重试", "busy"
		w.Header().Set("Retry-After", "2")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	failure(w, status, message)
	return true
}

func (s *Server) searchViews(r *http.Request, items []searchCandidate, kind string) ([]any, error) {
	views := make([]any, 0, len(items))
	stars, err := s.DB.StarredSet(r.Context(), currentUser(r).ID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		var view map[string]any
		switch kind {
		case "track":
			if item.local {
				view, err = s.trackView(r.Context(), item.track, stars[item.track.TrackID()] != 0)
			} else {
				view = s.searchTrackView(r.Context(), item.track, stars[item.track.TrackID()] != 0)
			}
		case "album", "artist":
			view, err = s.searchGroupView(r.Context(), item, kind)
		case "playlist":
			view, err = s.playlistView(r.Context(), item.playlist)
		}
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Server) searchSuggest(w http.ResponseWriter, r *http.Request) {
	event, finish := s.beginResourceDiagnostic(w, r, "search3", "json")
	defer finish()
	w, finishResponse := diagnostics.CaptureResponse(w, r, event)
	defer finishResponse()
	query, valid := searchQuery(w, r)
	if !valid {
		return
	}
	parent := r.Context()
	ctx, cancel := context.WithTimeout(parent, searchTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	if query != "" {
		release, err := s.Catalog.AcquireRequest(ctx, currentUser(r).ID)
		if searchFailure(w, event, err) {
			return
		}
		defer release()
	}
	kinds := []string{"track", "album", "artist", "playlist"}
	lists := make([][]searchCandidate, len(kinds))
	errorsByKind := make([]error, len(kinds))
	var tasks sync.WaitGroup
	for i, kind := range kinds {
		tasks.Go(func() { lists[i], errorsByKind[i] = s.searchCandidates(r, query, kind) })
	}
	tasks.Wait()
	if parent.Err() != nil {
		return
	}
	r = r.WithContext(parent)
	data := map[string]any{}
	var firstErr error
	count := 0
	for i, kind := range kinds {
		if firstErr == nil {
			firstErr = errorsByKind[i]
		}
		count += len(lists[i])
		views, err := s.searchViews(r, lists[i][:min(5, len(lists[i]))], kind)
		if resourceError(w, err) {
			return
		}
		data[kind] = map[string]any{"items": views, "total": len(lists[i])}
	}
	if count == 0 && searchFailure(w, event, firstErr) {
		return
	}
	event.Count, event.Result = &count, "ok"
	if count == 0 {
		event.Result = "empty"
	}
	success(w, data)
}
