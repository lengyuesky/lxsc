package fnmusic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

func identifier(kind, id string) string {
	sum := sha256.Sum256([]byte("lxsc:fnmusic:" + kind + ":" + id))
	return hex.EncodeToString(sum[:16])
}

func validGUID(guid string) bool {
	if len(guid) != 32 || strings.ToLower(guid) != guid {
		return false
	}
	_, err := hex.DecodeString(guid)
	return err == nil
}

func (s *Server) identify(ctx context.Context, kind, id string) (string, error) {
	guid := identifier(kind, id)
	return guid, s.DB.EnsureClientID(ctx, "fnmusic", kind, id, guid)
}

func (s *Server) resolve(ctx context.Context, kind, guid string) (string, error) {
	if !validGUID(guid) {
		return "", db.ErrNotFound
	}
	gotKind, id, err := s.DB.ResolveClientID(ctx, "fnmusic", guid)
	if err != nil || gotKind != kind {
		return "", db.ErrNotFound
	}
	return id, nil
}

func (s *Server) libraries(w http.ResponseWriter, r *http.Request) {
	guid := identifier("library", strconv.FormatInt(currentUser(r).ID, 10))
	success(w, map[string]any{"list": []any{map[string]any{
		"guid": guid, "name": "在线音乐库", "path": "", "metadataPreference": "",
		"autoDownloadLyric": false, "contentLastChangedAt": 0,
	}}, "total": 1})
}

func (s *Server) visibleTracks(r *http.Request) ([]*music.Info, error) {
	ids, err := s.DB.LibraryTrackIDs(r.Context(), currentUser(r).ID)
	if err != nil {
		return nil, err
	}
	return s.Catalog.Tracks(r.Context(), ids), nil
}

func (s *Server) allowedTrack(r *http.Request, guid string) (*music.Info, error) {
	track, _, err := s.trackAccess(r, guid)
	return track, err
}

func (s *Server) trackAccess(r *http.Request, guid string) (*music.Info, bool, error) {
	if resource, ok := s.searches.get(r.Context(), "track", guid, time.Now()); ok && resource.track != nil {
		s.Catalog.Cache([]*music.Info{resource.track})
		return resource.track, true, nil
	}
	id, err := s.resolve(r.Context(), "track", guid)
	if err != nil {
		return nil, false, err
	}
	allowed, err := s.DB.CanReadLibraryTrack(r.Context(), currentUser(r), id)
	if err != nil {
		return nil, false, err
	}
	if !allowed {
		return nil, false, db.ErrNotFound
	}
	track, err := s.Catalog.LocalTrack(r.Context(), id)
	return track, false, err
}

func (s *Server) allowedPlaylist(r *http.Request, guid string) (*db.Playlist, error) {
	id, err := s.resolve(r.Context(), "playlist", guid)
	if err != nil {
		return nil, err
	}
	playlist, err := s.DB.GetPlaylist(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if !db.CanReadPlaylist(currentUser(r), playlist) {
		return nil, db.ErrNotFound
	}
	return playlist, nil
}

func (s *Server) playlistView(ctx context.Context, playlist *db.Playlist) (map[string]any, error) {
	guid, err := s.identify(ctx, "playlist", playlist.ID)
	if err != nil {
		return nil, err
	}
	if playlist.TrackIDs == nil && playlist.Count > 0 {
		playlist, err = s.DB.GetPlaylist(ctx, playlist.ID)
		if err != nil {
			return nil, err
		}
	}
	tracks := s.Catalog.Tracks(ctx, playlist.TrackIDs)
	var coverTrack *music.Info
	if len(tracks) > 0 {
		coverTrack = tracks[0]
	}
	return map[string]any{
		"guid": guid, "name": playlist.Name, "coverId": s.coverID("playlist", guid, coverTrack),
		"trackCount": len(tracks), "createdAt": playlist.CreatedAt, "updatedAt": playlist.UpdatedAt,
	}, nil
}

func (s *Server) playlists(w http.ResponseWriter, r *http.Request) {
	playlists, err := s.DB.ListPlaylists(r.Context(), currentUser(r).ID)
	if err != nil {
		failure(w, http.StatusInternalServerError, "歌单读取失败")
		return
	}
	items := make([]any, 0, len(playlists))
	for _, playlist := range playlists {
		view, err := s.playlistView(r.Context(), playlist)
		if err != nil {
			failure(w, http.StatusInternalServerError, "歌单标识失败")
			return
		}
		items = append(items, view)
	}
	if r.URL.Query().Get("page") == "" && r.URL.Query().Get("size") == "" {
		success(w, map[string]any{"list": items, "total": len(items)})
		return
	}
	page(w, r, items)
}

func (s *Server) playlistDetail(w http.ResponseWriter, r *http.Request) {
	playlist, err := s.allowedPlaylist(r, r.URL.Query().Get("guid"))
	if resourceError(w, err) {
		return
	}
	view, err := s.playlistView(r.Context(), playlist)
	if resourceError(w, err) {
		return
	}
	success(w, view)
}

func (s *Server) playlistBatch(w http.ResponseWriter, r *http.Request) {
	guids := strings.Split(r.URL.Query().Get("guids"), ",")
	if len(guids) > 100 {
		failure(w, http.StatusBadRequest, "歌单数量超限")
		return
	}
	items := []any{}
	for _, guid := range guids {
		playlist, err := s.allowedPlaylist(r, guid)
		if resourceError(w, err) {
			return
		}
		view, err := s.playlistView(r.Context(), playlist)
		if resourceError(w, err) {
			return
		}
		items = append(items, view)
	}
	success(w, map[string]any{"list": items, "total": len(items)})
}

func (s *Server) playlistTracks(w http.ResponseWriter, r *http.Request) {
	playlist, err := s.allowedPlaylist(r, r.URL.Query().Get("playlistGUID"))
	if resourceError(w, err) {
		return
	}
	s.writeTracks(w, r, s.Catalog.Tracks(r.Context(), playlist.TrackIDs))
}

func (s *Server) tracks(w http.ResponseWriter, r *http.Request) {
	if s.searchGroupTracks(w, r) {
		return
	}
	tracks, err := s.visibleTracks(r)
	if resourceError(w, err) {
		return
	}
	filtered := tracks[:0]
	query := r.URL.Query()
	album, artist, genre := query.Get("albumGUID"), query.Get("artistGUID"), query.Get("genreGUID")
	for _, track := range tracks {
		if (album != "" && album != identifier("album", track.AlbumSubID())) ||
			(artist != "" && artist != identifier("artist", track.ArtistSubID())) || genre != "" {
			continue
		}
		filtered = append(filtered, track)
	}
	s.writeTracks(w, r, filtered)
}

func (s *Server) favorites(w http.ResponseWriter, r *http.Request) {
	items, err := s.DB.ListStarred(r.Context(), currentUser(r).ID, "track")
	if resourceError(w, err) {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ItemID)
	}
	s.writeTracks(w, r, s.Catalog.Tracks(r.Context(), ids))
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	ids, err := s.DB.RecentTracks(r.Context(), currentUser(r).ID, 200)
	if resourceError(w, err) {
		return
	}
	s.writeTracks(w, r, s.Catalog.Tracks(r.Context(), ids))
}

func (s *Server) writeTracks(w http.ResponseWriter, r *http.Request, tracks []*music.Info) {
	stars, err := s.DB.StarredSet(r.Context(), currentUser(r).ID)
	if resourceError(w, err) {
		return
	}
	appliedSort := sortTracks(r, tracks)
	items := make([]any, 0, len(tracks))
	start, end := pageBounds(r, len(tracks))
	for _, track := range tracks[start:end] {
		view, err := s.trackView(r.Context(), track, stars[track.TrackID()] != 0)
		if resourceError(w, err) {
			return
		}
		items = append(items, view)
	}
	success(w, map[string]any{"list": items, "total": len(tracks), "sort": appliedSort})
}

func sortTracks(r *http.Request, tracks []*music.Info) string {
	requested := r.URL.Query().Get("sort")
	field, direction, _ := strings.Cut(requested, ",")
	if (field != "name" && field != "title" && field != "duration") || (direction != "" && direction != "asc" && direction != "desc") {
		return ""
	}
	sort.SliceStable(tracks, func(i, j int) bool {
		order := strings.Compare(tracks[i].Name(), tracks[j].Name())
		if field == "duration" {
			order = tracks[i].Duration() - tracks[j].Duration()
		}
		if direction == "desc" {
			return order > 0
		}
		return order < 0
	})
	return requested
}

func (s *Server) trackView(ctx context.Context, track *music.Info, favorite bool) (map[string]any, error) {
	guid, err := s.identify(ctx, "track", track.TrackID())
	if err != nil {
		return nil, err
	}
	artistGUID, err := s.identify(ctx, "artist", track.ArtistSubID())
	if err != nil {
		return nil, err
	}
	albumGUID, err := s.identify(ctx, "album", track.AlbumSubID())
	if err != nil {
		return nil, err
	}
	return s.trackViewWithIDs(track, guid, albumGUID, artistGUID, favorite), nil
}

func (s *Server) trackViewWithIDs(track *music.Info, guid, albumGUID, artistGUID string, favorite bool) map[string]any {
	artists := []any{map[string]any{"guid": artistGUID, "name": track.Singer(), "coverId": s.coverID("artist", artistGUID, track)}}
	album := map[string]any{"guid": albumGUID, "name": track.Album(), "artists": artists, "coverId": s.coverID("album", albumGUID, track)}
	_, hasLyric := s.Catalog.CachedLyric(track)
	return map[string]any{
		"guid": guid, "title": track.Name(), "artists": artists, "album": album, "genres": []any{},
		"duration": track.Duration() * 1000, "audioSpec": audioSpec(track, guid), "coverId": s.coverID("track", guid, track),
		"isFavorite": favorite, "isCue": false, "hasLyric": hasLyric, "accessStatus": 0,
		"year": nil, "trackNo": nil, "discNo": nil, "createdAt": 0, "updatedAt": 0,
	}
}

func audioSpec(track *music.Info, guid string) map[string]any {
	spec := map[string]any{"duration": track.Duration() * 1000, "path": "/online/" + guid}
	// 只报告已知音源元数据，不估算或伪造文件属性。
	if known, ok := track.Raw["audioSpec"].(map[string]any); ok {
		for _, key := range []string{"format", "codec", "size", "sampleRate", "bitDepth", "channel", "duration", "bitrate"} {
			if value, exists := known[key]; exists {
				spec[key] = value
			}
		}
		if format, ok := spec["format"].(string); ok && format != "" && len(format) < 16 && !strings.ContainsAny(format, "/\\.") {
			spec["path"] = "/online/" + guid + "." + format
		}
	}
	return spec
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	track, transient, err := s.trackAccess(r, r.URL.Query().Get("guid"))
	if resourceError(w, err) {
		return
	}
	stars, err := s.DB.StarredSet(r.Context(), currentUser(r).ID)
	if resourceError(w, err) {
		return
	}
	if _, available := s.Catalog.CachedLyric(track); !available {
		// 只为当前曲目做有界懒加载，不让列表中的每首歌同步访问平台。
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		_, _ = s.loadLyric(ctx, currentUser(r).ID, track)
		cancel()
	}
	var view map[string]any
	if transient {
		view = s.trackViewWithIDs(track, identifier("track", track.TrackID()), identifier("album", track.AlbumSubID()), identifier("artist", track.ArtistSubID()), stars[track.TrackID()] != 0)
	} else {
		view, err = s.trackView(r.Context(), track, stars[track.TrackID()] != 0)
		if resourceError(w, err) {
			return
		}
	}
	success(w, map[string]any{"track": view, "audioSpec": view["audioSpec"]})
}

func (s *Server) albums(w http.ResponseWriter, r *http.Request)  { s.groups(w, r, "album") }
func (s *Server) artists(w http.ResponseWriter, r *http.Request) { s.groups(w, r, "artist") }

func (s *Server) groups(w http.ResponseWriter, r *http.Request, kind string) {
	if kind == "album" && s.searchArtistAlbums(w, r) {
		return
	}
	tracks, err := s.visibleTracks(r)
	if resourceError(w, err) {
		return
	}
	type group struct {
		id    string
		track *music.Info
		count int
	}
	groups := []*group{}
	seen := map[string]*group{}
	for _, track := range tracks {
		if filter := r.URL.Query().Get("artistGUID"); kind == "album" && filter != "" && filter != identifier("artist", track.ArtistSubID()) {
			continue
		}
		id := track.ArtistSubID()
		if kind == "album" {
			id = track.AlbumSubID()
		}
		if previous, ok := seen[id]; ok {
			previous.count++
		} else {
			item := &group{id: id, track: track, count: 1}
			seen[id] = item
			groups = append(groups, item)
		}
	}
	requested := r.URL.Query().Get("sort")
	field, direction, _ := strings.Cut(requested, ",")
	appliedSort := ""
	if (field == "name" || field == "trackCount") && (direction == "" || direction == "asc" || direction == "desc") {
		appliedSort = requested
		sort.SliceStable(groups, func(i, j int) bool {
			name := func(g *group) string {
				if kind == "album" {
					return g.track.Album()
				}
				return g.track.Singer()
			}
			order := strings.Compare(name(groups[i]), name(groups[j]))
			if field == "trackCount" {
				order = groups[i].count - groups[j].count
			}
			if direction == "desc" {
				return order > 0
			}
			return order < 0
		})
	}
	start, end := pageBounds(r, len(groups))
	items := make([]any, 0, end-start)
	for _, item := range groups[start:end] {
		guid, err := s.identify(r.Context(), kind, item.id)
		if resourceError(w, err) {
			return
		}
		name := item.track.Singer()
		view := map[string]any{"guid": guid, "coverId": s.coverID(kind, guid, item.track), "trackCount": item.count}
		if kind == "album" {
			name = item.track.Album()
			artistGUID, err := s.identify(r.Context(), "artist", item.track.ArtistSubID())
			if resourceError(w, err) {
				return
			}
			view["artists"] = []any{map[string]any{"guid": artistGUID, "name": item.track.Singer()}}
		}
		view["name"] = name
		items = append(items, view)
	}
	success(w, map[string]any{"list": items, "total": len(groups), "sort": appliedSort})
}

func resourceError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, db.ErrNotFound) {
		failure(w, http.StatusNotFound, "资源不存在或无权访问")
	} else {
		failure(w, http.StatusInternalServerError, "音乐数据读取失败")
	}
	return true
}

func page(w http.ResponseWriter, r *http.Request, items []any) {
	appliedSort := sortItems(r, items)
	start, end := pageBounds(r, len(items))
	success(w, map[string]any{"list": items[start:end], "total": len(items), "sort": appliedSort})
}

func sortItems(r *http.Request, items []any) string {
	requested := r.URL.Query().Get("sort")
	field, direction, _ := strings.Cut(requested, ",")
	switch field {
	case "name", "title", "duration", "trackCount", "createdAt", "updatedAt", "newTrackAddedAt":
	default:
		return ""
	}
	if direction != "" && direction != "asc" && direction != "desc" {
		return ""
	}
	if field == "newTrackAddedAt" {
		field = "updatedAt"
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, aOK := items[i].(map[string]any)
		b, bOK := items[j].(map[string]any)
		if !aOK || !bOK {
			return false
		}
		order := 0
		if first, ok := a[field].(string); ok {
			second, _ := b[field].(string)
			order = strings.Compare(first, second)
		} else {
			first, _ := a[field].(int64)
			second, _ := b[field].(int64)
			if v, ok := a[field].(int); ok {
				first = int64(v)
			}
			if v, ok := b[field].(int); ok {
				second = int64(v)
			}
			if first < second {
				order = -1
			}
			if first > second {
				order = 1
			}
		}
		if direction == "desc" {
			return order > 0
		}
		return order < 0
	})
	return requested
}

func pageBounds(r *http.Request, total int) (int, int) {
	query := r.URL.Query()
	number, err := strconv.Atoi(query.Get("page"))
	if err != nil || number < 1 {
		number = 1
	}
	size, err := strconv.Atoi(query.Get("size"))
	if err != nil || size < 1 {
		size = 100
	}
	size = min(size, 500)
	start := total
	if number-1 <= total/size {
		start = min((number-1)*size, total)
	}
	return start, min(start+size, total)
}
