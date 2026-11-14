package fnmusic

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/music"
)

// 标准音质偏好会让原版客户端先准备播放，再读取固定的 preset.m3u8。
// 这里只提供指向原始流的播放清单，不创建转码任务或改变用户的取链音质。
func (s *Server) prepareOriginalPlayback(w http.ResponseWriter, r *http.Request) {
	guid, ok := originalPlaybackGUID(w, r)
	if !ok {
		return
	}
	if _, ok := s.originalPlaybackTrack(w, r, guid); !ok {
		return
	}
	success(w, map[string]any{"guid": guid, "status": "ready", "transcoded": false})
}

// 心跳和退出仅确认当前用户的曲目权限，不分配任务，也不增加播放或收听统计。
func (s *Server) originalPlaybackControl(w http.ResponseWriter, r *http.Request) {
	guid, ok := originalPlaybackGUID(w, r)
	if !ok {
		return
	}
	if _, err := s.allowedTrack(r, guid); resourceError(w, err) {
		return
	}
	success(w, map[string]any{"guid": guid, "status": "success", "transcoded": false})
}

func originalPlaybackGUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		return r.URL.Query().Get("guid"), true
	}
	var body struct {
		GUID string `json:"guid"`
	}
	if !decodeBody(w, r, &body) {
		return "", false
	}
	return body.GUID, true
}

func (s *Server) originalPlaybackTrack(w http.ResponseWriter, r *http.Request, guid string) (*music.Info, bool) {
	track, err := s.allowedTrack(r, guid)
	if resourceError(w, err) {
		return nil, false
	}
	if s.Media == nil {
		failure(w, http.StatusServiceUnavailable, "音乐交付未配置")
		return nil, false
	}
	if track.Duration() <= 0 {
		failure(w, http.StatusUnprocessableEntity, "曲目时长未知，请选择原始音质播放")
		return nil, false
	}
	return track, true
}

func (s *Server) originalPlaybackPlaylist(w http.ResponseWriter, r *http.Request) {
	guid := chi.URLParam(r, "guid")
	track, ok := s.originalPlaybackTrack(w, r, guid)
	if !ok {
		return
	}
	// 单个播放项就是完整原文件；实际正文继续由共享媒体服务处理鉴权、
	// 取链、解码验证、302/代理和 Range，不把原始音频字节冒充播放清单。
	duration := track.Duration()
	playlist := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:%d.0,\n/music/api/v1/track/stream?guid=%s\n#EXT-X-ENDLIST\n", duration, duration, guid)
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != http.MethodHead {
		_, _ = fmt.Fprint(w, playlist)
	}
}
