package portal

import (
	"context"
	"errors"
	"net/http"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

func uniqueTrackIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// resolvePlaylistTracks 只解析元数据；调用者随后与歌单关系一起提交，不提前落库。
func (s *Server) resolvePlaylistTracks(w http.ResponseWriter, r *http.Request, ids []string) ([]db.Track, bool) {
	if len(ids) > maxPlaylistTracks {
		playlistWriteError(w, db.ErrPlaylistLimit)
		return nil, false
	}
	for _, id := range ids {
		parsed, valid := music.ParseID(id)
		if !valid || parsed.Kind != music.KindTrack || !music.IsPlatform(parsed.Source) || parsed.Key == "" || len(id) > 512 {
			fail(w, http.StatusBadRequest, "无效的歌曲 ID")
			return nil, false
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	tracks := make([]db.Track, 0, len(ids))
	for _, id := range uniqueTrackIDs(ids) {
		in, err := s.Catalog.Track(ctx, id)
		if err != nil {
			fail(w, http.StatusBadRequest, "找不到歌曲，请重新搜索后再试")
			return nil, false
		}
		// 按歌单实际使用的 ID 落库，保留酷狗等平台的旧别名。
		tracks = append(tracks, db.Track{ID: id, Source: in.Source(), Name: in.Name(), Singer: in.Singer(), Album: in.Album(), JSON: in.JSON()})
	}
	return tracks, true
}

func playlistWriteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		fail(w, http.StatusNotFound, "歌单不存在")
	case errors.Is(err, db.ErrPlaylistForbidden):
		fail(w, http.StatusForbidden, err.Error())
	case errors.Is(err, db.ErrPlaylistConflict):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, db.ErrPlaylistLimit):
		fail(w, http.StatusBadRequest, err.Error())
	default:
		fail(w, http.StatusInternalServerError, "保存歌单失败，请稍后重试")
	}
}

func (s *Server) addPlaylistTrack(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	var body struct {
		TrackID string `json:"trackId"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	tracks, ok := s.resolvePlaylistTracks(w, r, []string{body.TrackID})
	if !ok {
		return
	}
	added, err := s.DB.PrependPlaylistTrack(r.Context(), p.ID, currentUser(r), tracks[0])
	if err != nil {
		playlistWriteError(w, err)
		return
	}
	updated, err := s.DB.GetPlaylist(r.Context(), p.ID)
	if err != nil {
		playlistWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"playlist": s.detail(r.Context(), currentUser(r), updated), "added": added})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if s.Stream == nil {
		fail(w, http.StatusServiceUnavailable, "播放服务未初始化")
		return
	}
	s.Stream(w, r, currentUser(r))
}
