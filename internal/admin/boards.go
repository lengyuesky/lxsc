package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"lxsc/internal/music"
)

// getBoards 返回完整候选目录，不受展示开关和已选榜单限制，也不加载歌曲。
func (s *Server) getBoards(w http.ResponseWriter, r *http.Request) {
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if !music.IsPlatform(source) {
		fail(w, http.StatusBadRequest, "请选择有效的榜单平台")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	boards, err := s.Catalog.Boards(ctx, source)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("读取可选榜单失败", "source", source, "err", err)
		}
		fail(w, http.StatusBadGateway, "读取"+music.PlatformName(source)+"榜单失败，请稍后重试")
		return
	}
	writeJSON(w, http.StatusOK, boards)
}
