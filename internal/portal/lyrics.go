package portal

import (
	"context"
	"net/http"
	"time"
)

func (s *Server) lyrics(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if len(id) == 0 || len(id) > 1024 {
		fail(w, 400, "歌曲ID无效")
		return
	}
	release, ok := s.admitSearch(w, r)
	if !ok {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	in, err := s.Catalog.Track(ctx, id)
	if err != nil {
		fail(w, 404, "歌曲不存在")
		return
	}
	lyrics, err := s.Catalog.Lyric(ctx, in)
	if err != nil {
		fail(w, 502, "歌词读取失败，请稍后重试")
		return
	}
	if lyrics == nil {
		writeJSON(w, 200, map[string]any{"lines": []any{}, "trans": []any{}, "synced": false, "offset": 0})
		return
	}
	writeJSON(w, 200, map[string]any{"lines": lyrics.Lines, "trans": lyrics.Trans, "synced": lyrics.Synced, "offset": lyrics.Offset})
}
