package fnmusic

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
	"lxsc/internal/secret"
)

const playlistWriteBodyLimit = 128 << 10 // 可容纳 2000 个 32 字节 GUID，登录限制保持原值。

func (s *Server) writeRoute(endpoint string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		event, finish := s.beginResourceDiagnostic(w, r, endpoint, "json")
		event.Result = "ok"
		defer finish()
		w, finishResponse := diagnostics.CaptureResponse(w, r, event)
		defer finishResponse()
		handler(w, r)
	}
}

func mutationError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		failure(w, http.StatusNotFound, "资源不存在或无权访问")
	case errors.Is(err, db.ErrPlaylistForbidden):
		failure(w, http.StatusForbidden, "无权修改该歌单")
	case errors.Is(err, db.ErrPlaylistConflict):
		failure(w, http.StatusConflict, "歌单已更新，请刷新后重试")
	case errors.Is(err, db.ErrPlaylistLimit):
		failure(w, http.StatusBadRequest, "单个歌单最多包含 2000 首歌曲")
	case errors.Is(err, db.ErrCoverLimit):
		failure(w, http.StatusTooManyRequests, "待使用封面数量超限，请稍后重试")
	default:
		failure(w, http.StatusInternalServerError, "音乐数据保存失败")
	}
	return true
}

func playlistName(w http.ResponseWriter, value string) (string, bool) {
	name := strings.TrimSpace(value)
	if count := utf8.RuneCountInString(name); count == 0 || count > 100 {
		failure(w, http.StatusBadRequest, "歌单名称必须为 1–100 个字符")
		return "", false
	}
	return name, true
}

func protocolID(kind, id string) db.ClientID {
	return db.ClientID{Protocol: "fnmusic", Kind: kind, ID: id, GUID: identifier(kind, id)}
}

func (s *Server) editablePlaylist(r *http.Request, guid string) (*db.Playlist, error) {
	playlist, err := s.allowedPlaylist(r, guid)
	if err != nil {
		return nil, err
	}
	if !db.CanEditPlaylist(currentUser(r), playlist) {
		return nil, db.ErrPlaylistForbidden
	}
	return playlist, nil
}

// 新上传的封面由写事务验证归属；编辑时也允许回传当前自动封面 ID。
func (s *Server) requestedCover(w http.ResponseWriter, r *http.Request, cover *string, playlist *db.Playlist) (*string, bool) {
	if cover == nil || *cover == "" || validGUID(*cover) {
		return cover, true
	}
	if playlist != nil {
		tracks := s.Catalog.Tracks(r.Context(), playlist.TrackIDs)
		var first *music.Info
		if len(tracks) > 0 {
			first = tracks[0]
		}
		if *cover == s.coverID("playlist", identifier("playlist", playlist.ID), first) {
			return nil, true
		}
	}
	failure(w, http.StatusBadRequest, "封面标识无效")
	return nil, false
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string  `json:"name"`
		CoverID *string `json:"coverId"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name, ok := playlistName(w, body.Name)
	if !ok {
		return
	}
	cover, ok := s.requestedCover(w, r, body.CoverID, nil)
	if !ok {
		return
	}
	coverID := ""
	if cover != nil {
		coverID = *cover
	}
	id := music.KindPlaylist + "-" + secret.RandomToken(9)
	mapping := protocolID("playlist", id)
	// 飞牛新建默认私有，不继承全站公开设置或客户端提供的归属用户。
	if mutationError(w, s.DB.CreatePlaylistWithAssets(r.Context(), id, currentUser(r).ID, name, "", false, nil, nil, coverID, mapping)) {
		return
	}
	success(w, map[string]any{"guid": mapping.GUID})
}

func (s *Server) editPlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GUID    string  `json:"guid"`
		Name    string  `json:"name"`
		CoverID *string `json:"coverId"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name, ok := playlistName(w, body.Name)
	if !ok {
		return
	}
	playlist, err := s.editablePlaylist(r, body.GUID)
	if mutationError(w, err) {
		return
	}
	cover, ok := s.requestedCover(w, r, body.CoverID, playlist)
	if !ok {
		return
	}
	if mutationError(w, s.DB.UpdatePlaylistPresentationChecked(r.Context(), playlist.ID, currentUser(r), name, cover)) {
		return
	}
	success(w, map[string]any{"guid": body.GUID})
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GUID string `json:"guid"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	playlist, err := s.editablePlaylist(r, body.GUID)
	if mutationError(w, err) {
		return
	}
	if mutationError(w, s.DB.DeletePlaylistChecked(r.Context(), playlist.ID, currentUser(r))) {
		return
	}
	success(w, map[string]any{"guid": body.GUID})
}

type playlistTrackRequest struct {
	GUID       string   `json:"guid"`
	TrackGUIDs []string `json:"trackGUIDs"`
}

func decodePlaylistTracks(w http.ResponseWriter, r *http.Request, body *playlistTrackRequest) bool {
	if !decodeBodyLimit(w, r, body, playlistWriteBodyLimit) {
		return false
	}
	if len(body.TrackGUIDs) == 0 || len(body.TrackGUIDs) > db.MaxPlaylistTracks {
		failure(w, http.StatusBadRequest, "请选择 1–2000 首歌曲")
		return false
	}
	return true
}

func (s *Server) writeTracksFor(r *http.Request, guids []string) ([]db.Track, []db.ClientID, db.TrackWriteAuthority, error) {
	tracks := make([]db.Track, 0, len(guids))
	mappings := make([]db.ClientID, 0, len(guids))
	seen := map[string]bool{}
	authority := db.TrackWriteAuthority{Actor: currentUser(r)}
	transientGUIDs := []string{}
	for _, guid := range guids {
		if seen[guid] {
			continue
		}
		track, transient, err := s.trackAccess(r, guid)
		if err != nil {
			return nil, nil, authority, err
		}
		id := track.TrackID()
		if transient {
			transientGUIDs = append(transientGUIDs, guid)
		} else {
			id, err = s.resolve(r.Context(), "track", guid)
			if err != nil {
				return nil, nil, authority, err
			}
			authority.LibraryIDs = append(authority.LibraryIDs, id)
		}
		seen[guid] = true
		tracks = append(tracks, db.Track{ID: id, Source: track.Source(), Name: track.Name(), Singer: track.Singer(), Album: track.Album(), JSON: track.JSON()})
		for _, mapping := range []db.ClientID{protocolID("track", id), protocolID("album", track.AlbumSubID()), protocolID("artist", track.ArtistSubID())} {
			key := mapping.Kind + ":" + mapping.ID
			if !seen[key] {
				mappings = append(mappings, mapping)
				seen[key] = true
			}
		}
	}
	authority.Check = func() error {
		token, ok := requestToken(r)
		entry, valid := s.sessions.get(token, time.Now())
		if !ok || !valid || entry.userID != currentUser(r).ID || entry.password != sha256.Sum256([]byte(currentUser(r).PasswordEnc)) {
			return db.ErrNotFound
		}
		for _, guid := range transientGUIDs {
			if _, valid := s.searches.get(r.Context(), "track", guid, time.Now()); !valid {
				return db.ErrNotFound
			}
		}
		return nil
	}
	return tracks, mappings, authority, nil
}

func (s *Server) addPlaylistTracks(w http.ResponseWriter, r *http.Request) {
	var body playlistTrackRequest
	if !decodePlaylistTracks(w, r, &body) {
		return
	}
	playlist, err := s.editablePlaylist(r, body.GUID)
	if mutationError(w, err) {
		return
	}
	tracks, mappings, authority, err := s.writeTracksFor(r, body.TrackGUIDs)
	if mutationError(w, err) {
		return
	}
	added, err := s.DB.PrependPlaylistTracksAuthorized(r.Context(), playlist.ID, currentUser(r), tracks, authority, mappings...)
	if mutationError(w, err) {
		return
	}
	success(w, map[string]any{"guid": body.GUID, "added": added})
}

func (s *Server) removePlaylistTracks(w http.ResponseWriter, r *http.Request) {
	var body playlistTrackRequest
	if !decodePlaylistTracks(w, r, &body) {
		return
	}
	playlist, err := s.editablePlaylist(r, body.GUID)
	if mutationError(w, err) {
		return
	}
	ids := make([]string, 0, len(body.TrackGUIDs))
	for _, guid := range body.TrackGUIDs {
		id, err := s.resolve(r.Context(), "track", guid)
		if mutationError(w, err) {
			return
		}
		ids = append(ids, id)
	}
	removed, err := s.DB.RemovePlaylistTracks(r.Context(), playlist.ID, currentUser(r), ids)
	if mutationError(w, err) {
		return
	}
	success(w, map[string]any{"guid": body.GUID, "removed": removed})
}

func (s *Server) favoriteTrack(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TrackGUID string `json:"trackGUID"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	tracks, mappings, authority, err := s.writeTracksFor(r, []string{body.TrackGUID})
	if mutationError(w, err) {
		return
	}
	if mutationError(w, s.DB.StarTracksAuthorized(r.Context(), currentUser(r).ID, tracks, authority, mappings...)) {
		return
	}
	success(w, map[string]any{"trackGUID": body.TrackGUID})
}

func (s *Server) unfavoriteTrack(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TrackGUID string `json:"trackGUID"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	// 取消收藏可幂等重试，即使第一次操作已经移除了最后一个可读引用。
	id, err := s.resolve(r.Context(), "track", body.TrackGUID)
	if mutationError(w, err) {
		return
	}
	if mutationError(w, s.DB.UnstarTracks(r.Context(), currentUser(r).ID, []string{id})) {
		return
	}
	success(w, map[string]any{"trackGUID": body.TrackGUID})
}
