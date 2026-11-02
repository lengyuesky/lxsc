package fnmusic

import (
	"context"
	"net/http"
	"strings"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
)

func albumFromTrack(track *music.Info) music.AlbumMeta {
	return music.AlbumMeta{Source: track.Source(), ID: track.AlbumID(), Name: track.Album(), Artist: track.PrimarySinger(), ArtistID: track.SingerID(), Image: track.Img()}
}

func artistFromTrack(track *music.Info) music.ArtistRef {
	return music.ArtistRef{Source: track.Source(), ID: track.SingerID(), Name: track.PrimarySinger()}
}

func (s *Server) searchTrackView(ctx context.Context, track *music.Info, favorite bool) map[string]any {
	guid := identifier("track", track.TrackID())
	albumGUID, artistGUID := identifier("album", track.AlbumSubID()), identifier("artist", track.ArtistSubID())
	now := time.Now()
	s.searches.grant(ctx, "track", guid, searchResource{track: track}, now)
	if _, known := s.searches.get(ctx, "album", albumGUID, now); !known {
		meta := albumFromTrack(track)
		s.searches.grant(ctx, "album", albumGUID, searchResource{album: &meta}, now)
	}
	if _, known := s.searches.get(ctx, "artist", artistGUID, now); !known {
		ref := artistFromTrack(track)
		s.searches.grant(ctx, "artist", artistGUID, searchResource{artist: &ref}, now)
	}
	return s.trackViewWithIDs(track, guid, albumGUID, artistGUID, favorite)
}

func (s *Server) searchGroupView(ctx context.Context, item searchCandidate, kind string) (map[string]any, error) {
	id, name := "", ""
	resource := searchResource{album: item.album, artist: item.artist}
	if kind == "album" {
		id, name = music.AlbumID(item.album.Source, item.album.ID, item.album.Name, item.album.Artist), item.album.Name
	} else {
		id, name = music.ArtistID(item.artist.Name), item.artist.Name
	}
	guid := identifier(kind, id)
	if item.local {
		var err error
		guid, err = s.identify(ctx, kind, id)
		if err != nil {
			return nil, err
		}
	} else {
		s.searches.grant(ctx, kind, guid, resource, time.Now())
	}
	view := map[string]any{"guid": guid, "name": name, "trackCount": item.count}
	if kind == "album" {
		meta := item.album
		artistGUID := identifier("artist", music.ArtistID(meta.Artist))
		if item.local {
			var err error
			artistGUID, err = s.identify(ctx, "artist", music.ArtistID(meta.Artist))
			if err != nil {
				return nil, err
			}
		} else if meta.Artist != "" {
			ref := music.ArtistRef{Source: meta.Source, ID: meta.ArtistID, Name: meta.Artist}
			if _, known := s.searches.get(ctx, "artist", artistGUID, time.Now()); !known {
				s.searches.grant(ctx, "artist", artistGUID, searchResource{artist: &ref}, time.Now())
			}
		}
		view["artists"] = []any{map[string]any{"guid": artistGUID, "name": meta.Artist}}
		view["coverId"] = s.coverVersionID(kind, guid, meta.Source, meta.ID, meta.Name, meta.Artist, meta.Image)
	} else {
		ref := item.artist
		view["albumCount"] = ref.AlbumSize
		view["coverId"] = s.coverVersionID(kind, guid, ref.Source, ref.ID, ref.Name, ref.Avatar)
	}
	return view, nil
}

func (s *Server) albumDetail(w http.ResponseWriter, r *http.Request)  { s.groupDetail(w, r, "album") }
func (s *Server) artistDetail(w http.ResponseWriter, r *http.Request) { s.groupDetail(w, r, "artist") }

func (s *Server) groupDetail(w http.ResponseWriter, r *http.Request, kind string) {
	guid := r.URL.Query().Get("guid")
	if resource, found := s.searches.get(r.Context(), kind, guid, time.Now()); found {
		item := searchCandidate{album: resource.album, artist: resource.artist}
		if resource.album != nil {
			item.count = resource.album.SongCount
		}
		view, err := s.searchGroupView(r.Context(), item, kind)
		if !resourceError(w, err) {
			success(w, view)
		}
		return
	}
	tracks, err := s.visibleTracks(r)
	if resourceError(w, err) {
		return
	}
	var item searchCandidate
	for _, track := range tracks {
		id := track.AlbumSubID()
		if kind == "artist" {
			id = track.ArtistSubID()
		}
		if identifier(kind, id) != guid {
			continue
		}
		item.count++
		if item.track == nil {
			meta, ref := albumFromTrack(track), artistFromTrack(track)
			item.track, item.album, item.artist, item.local = track, &meta, &ref, true
		}
	}
	if item.track == nil {
		resourceError(w, db.ErrNotFound)
		return
	}
	view, err := s.searchGroupView(r.Context(), item, kind)
	if !resourceError(w, err) {
		success(w, view)
	}
}

func (s *Server) searchGroupTracks(w http.ResponseWriter, r *http.Request) bool {
	guid, kind := r.URL.Query().Get("albumGUID"), "album"
	if guid == "" {
		guid, kind = r.URL.Query().Get("artistGUID"), "artist"
	}
	resource, found := s.searches.get(r.Context(), kind, guid, time.Now())
	if !found {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	release, err := s.Catalog.AcquireRequest(ctx, currentUser(r).ID)
	if searchFailure(w, &diagnostics.Event{}, err) {
		return true
	}
	defer release()
	var tracks []*music.Info
	if kind == "album" && resource.album != nil {
		meta := resource.album
		if meta.ID != "" && meta.ID != "0" {
			tracks, _, err = s.Catalog.AlbumSongsFor(ctx, music.AlbumLocator{Source: meta.Source, ID: meta.ID, Name: meta.Name, Artist: meta.Artist, Image: meta.Image})
		} else {
			tracks, err = s.remoteSearchTracks(ctx, strings.TrimSpace(meta.Name+" "+meta.Artist), music.SearchOptions{Sources: []string{meta.Source}, Page: 1, Limit: 50})
			filtered := tracks[:0]
			for _, track := range tracks {
				if identifier("album", track.AlbumSubID()) == guid {
					filtered = append(filtered, track)
				}
			}
			tracks = filtered
		}
	} else if resource.artist != nil {
		ref := resource.artist
		tracks, err = s.remoteSearchTracks(ctx, ref.Name, music.SearchOptions{Sources: []string{ref.Source}, Page: 1, Limit: 50})
		filtered := tracks[:0]
		for _, track := range tracks {
			if strings.EqualFold(strings.TrimSpace(track.PrimarySinger()), strings.TrimSpace(ref.Name)) {
				filtered = append(filtered, track)
			}
		}
		tracks = filtered
	}
	if searchFailure(w, &diagnostics.Event{}, err) {
		return true
	}
	items := make([]searchCandidate, 0, min(500, len(tracks)))
	seen := map[string]bool{}
	for _, track := range tracks {
		if track == nil || !diagnostics.ValidTrackID(track.TrackID()) || seen[track.TrackID()] {
			continue
		}
		if resource.album != nil && track.Source() != resource.album.Source {
			continue
		}
		query := r.URL.Query()
		if query.Get("genreGUID") != "" || (kind == "album" && query.Get("artistGUID") != "" && identifier("artist", track.ArtistSubID()) != query.Get("artistGUID")) {
			continue
		}
		seen[track.TrackID()] = true
		items = append(items, searchCandidate{track: track})
		if len(items) == 500 {
			break
		}
	}
	start, end := pageBounds(r, len(items))
	views, err := s.searchViews(r, items[start:end], "track")
	if !resourceError(w, err) {
		success(w, map[string]any{"list": views, "total": len(items), "sort": ""})
	}
	return true
}

func (s *Server) searchArtistAlbums(w http.ResponseWriter, r *http.Request) bool {
	resource, found := s.searches.get(r.Context(), "artist", r.URL.Query().Get("artistGUID"), time.Now())
	if !found || resource.artist == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	release, err := s.Catalog.AcquireRequest(ctx, currentUser(r).ID)
	if searchFailure(w, &diagnostics.Event{}, err) {
		return true
	}
	defer release()
	// Core owns platform-specific album pagination; do not scan an artist's full catalogue here.
	page, err := s.Catalog.ArtistAlbums(ctx, *resource.artist, 1)
	if searchFailure(w, &diagnostics.Event{}, err) {
		return true
	}
	items := make([]searchCandidate, 0, len(page.List))
	for _, meta := range page.List {
		items = append(items, searchCandidate{album: &meta, count: meta.SongCount})
	}
	start, end := pageBounds(r, len(items))
	views, err := s.searchViews(r, items[start:end], "album")
	if !resourceError(w, err) {
		success(w, map[string]any{"list": views, "total": len(items), "sort": ""})
	}
	return true
}
