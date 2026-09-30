package portal

import (
	"github.com/go-chi/chi/v5"
	"net/http"
	"strconv"
)

func (s *Server) playlistHistory(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	history, err := s.DB.PlaylistHistory(r.Context(), p.ID, currentUser(r))
	if err != nil {
		playlistWriteError(w, err)
		return
	}
	writeJSON(w, 200, history)
}
func (s *Server) playlistHistoryTracks(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadPlaylistForEdit(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "historyID"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "历史记录ID无效")
		return
	}
	ids, err := s.DB.PlaylistHistoryTracks(r.Context(), p.ID, id, currentUser(r))
	if err != nil {
		playlistWriteError(w, err)
		return
	}
	tracks := make([]trackView, 0, len(ids))
	for _, id := range ids {
		// 元数据仍受历史列表引用保护，预览不请求外部平台。
		in, err := s.Catalog.LocalTrack(r.Context(), id)
		if err != nil {
			tracks = append(tracks, trackView{ID: id, Name: "无法读取的歌曲", Unavailable: true})
			continue
		}
		track := infoTrackView(in)
		track.ID = id
		tracks = append(tracks, track)
	}
	writeJSON(w, 200, tracks)
}
