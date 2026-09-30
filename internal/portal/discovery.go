package portal

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func (s *Server) smartTracks(w http.ResponseWriter, r *http.Request) {
	kind, singer := r.URL.Query().Get("kind"), strings.TrimSpace(r.URL.Query().Get("singer"))
	if kind != "frequent" && kind != "month" && kind != "rediscover" && kind != "singer" {
		fail(w, 400, "智能歌单类型无效")
		return
	}
	if kind == "singer" && (singer == "" || len(singer) > 200) {
		fail(w, 400, "请输入歌手名称")
		return
	}
	release, ok := s.admitSearch(w, r)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ids, err := s.DB.SmartTrackIDs(ctx, currentUser(r).ID, kind, singer)
	if err != nil {
		fail(w, 500, "读取智能歌单失败")
		return
	}
	tracks := []trackView{}
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if in, err := s.Catalog.Track(ctx, id); err == nil {
			tracks = append(tracks, infoTrackView(in))
		}
	}
	writeJSON(w, 200, tracks)
}
