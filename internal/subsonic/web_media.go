package subsonic

import (
	"encoding/json"
	"net/http"
	"net/url"

	"lxsc/internal/db"
	"lxsc/internal/music"
)

// ServeWebStream 仅供已完成 Cookie 认证的网页入口调用，不改变 /rest 的认证契约。
func (s *Server) ServeWebStream(w http.ResponseWriter, r *http.Request, user *db.User) {
	if user == nil {
		writeWebMediaError(w, r, ErrWrongAuth, "未登录或会话已过期")
		return
	}
	id := r.URL.Query().Get("id")
	parsed, valid := music.ParseID(id)
	if !valid || parsed.Kind != music.KindTrack || !music.IsPlatform(parsed.Source) || parsed.Key == "" || len(id) > 512 {
		writeWebMediaError(w, r, ErrMissingParam, "无效的歌曲 ID")
		return
	}
	quality := r.URL.Query().Get("quality")
	switch quality {
	case "", "128k", "320k", "flac", "flac24bit":
	default:
		writeWebMediaError(w, r, ErrMissingParam, "不支持的播放音质")
		return
	}
	// 只传递网页契约中的参数，不接受 proxy 或其他 Subsonic 参数覆盖服务器设置。
	request := r.Clone(withUser(r.Context(), user))
	query := url.Values{"id": {id}}
	if quality != "" {
		query.Set("quality", quality)
	}
	request.URL.RawQuery = query.Encode()
	s.serveMediaWithError(w, request, true, writeWebMediaError)
}

func writeWebMediaError(w http.ResponseWriter, _ *http.Request, code int, message string) {
	status := http.StatusBadGateway
	switch code {
	case ErrWrongAuth:
		status = http.StatusUnauthorized
	case ErrMissingParam:
		status = http.StatusBadRequest
	case ErrNotFound:
		status = http.StatusNotFound
		message = "找不到歌曲，请重新搜索后再试"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
