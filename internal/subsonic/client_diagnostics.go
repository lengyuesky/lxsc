package subsonic

import (
	"context"
	"crypto/rand"
	"net/http"
	"strings"
	"time"

	"lxsc/internal/diagnostics"
)

type clientDiagnosticKey struct{}

// 只摘取响应的数量和状态，不保存请求正文、认证参数或任何歌曲/歌词文本。
func (s *Server) beginClientDiagnostic(r *http.Request, endpoint string) (*http.Request, func()) {
	switch endpoint {
	case "getPlaylists", "getPlaylist", "getMusicDirectory", "getLyrics", "getLyricsBySongId", "getSong", "getAlbum", "stream", "download":
	default:
		return r, func() {}
	}
	if s.Diagnostics == nil {
		return r, func() {}
	}
	started := time.Now()
	client := strings.ToLower(param(r, "c"))
	category := "other"
	switch {
	case client == "":
		category = "unknown"
	case strings.Contains(client, "amcfy") || strings.Contains(client, "musichub") || strings.Contains(client, "箭头"):
		category = "amcfy"
	case strings.Contains(client, "streammusic") || strings.Contains(client, "stream music") || strings.Contains(client, "音流"):
		category = "stream_music"
	}
	// 独立生成关联 ID，绝不信任或记录客户端传入的请求 ID/认证信息。
	event := &diagnostics.Event{RequestID: "req-" + rand.Text(), Stage: "client_response", Endpoint: endpoint, Method: r.Method, Client: category, Format: detectFormat(r), Result: "unavailable", Error: "none"}
	id := param(r, "id")
	if diagnostics.ValidTrackID(id) {
		event.TrackID = id
	}
	if diagnostics.ValidBoardID(id) {
		event.BoardID = id
	}
	return r.WithContext(context.WithValue(r.Context(), clientDiagnosticKey{}, event)), func() {
		event.ElapsedMS = time.Since(started).Milliseconds()
		if r.Context().Err() != nil {
			if event.Error == "none" {
				event.Error = diagnostics.ErrorCode(r.Context().Err())
			}
			event.Result = "failed"
		}
		s.Diagnostics.Add(*event)
	}
}

func recordClientDelivery(r *http.Request, written, expected int, issue string) {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	if event == nil {
		return
	}
	n, total := int64(written), int64(expected)
	event.BytesWritten, event.ResponseBytes = &n, &total
	if issue != "" {
		event.Result, event.Error = "failed", issue
	}
}

func recordMediaResponse(r *http.Request, status int) {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	if event == nil {
		return
	}
	event.Status, event.Format, event.Result = status, "binary", "ok"
	if status >= http.StatusBadRequest {
		event.Result = "failed"
	}
}

func recordClientResponse(r *http.Request, status, name string, payload any, errObj M) {
	event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	if event == nil {
		return
	}
	event.Result = status
	if event.Status == 0 {
		event.Status = 200
	} // 常规 GET/POST 协议失败仍以 HTTP 200 返回；HEAD、限流、繁忙使用实际 HTTP 错误状态。
	if errObj != nil {
		if code, ok := errObj["code"].(int); ok {
			event.ProtocolCode = &code
		}
		return
	}
	obj, _ := payload.(M)
	switch name {
	case "playlists":
		if list, ok := obj["playlist"].([]M); ok {
			n := len(list)
			event.Count = &n
			for _, item := range list {
				id, _ := item["id"].(string)
				if diagnostics.ValidBoardID(id) {
					event.BoardIDs = append(event.BoardIDs, id)
				}
				if len(event.BoardIDs) == 10 {
					break
				}
			}
		}
	case "playlist":
		if list, ok := obj["entry"].([]M); ok {
			n := len(list)
			event.Count = &n
		}
	case "directory":
		if list, ok := obj["child"].([]M); ok {
			n := len(list)
			event.Count = &n
		}
	case "album":
		if list, ok := obj["song"].([]M); ok {
			n := len(list)
			event.Count = &n
		}
	case "lyrics":
		if value, ok := obj["value"].(string); ok {
			n := 0
			if strings.TrimSpace(value) != "" {
				n = len(strings.Split(value, "\n"))
			}
			event.Lines = &n
		}
	case "lyricsList":
		if list, ok := obj["structuredLyrics"].([]M); ok {
			n := len(list)
			event.Count = &n
			lines := 0
			synced := false
			for _, lyric := range list {
				if values, ok := lyric["line"].([]M); ok {
					lines += len(values)
				}
				if lyric["synced"] == true {
					synced = true
				}
			}
			event.Lines = &lines
			event.Synced = &synced
		}
	}
	if (event.Count != nil && *event.Count == 0) || (event.Lines != nil && *event.Lines == 0) {
		event.Result = "empty"
	}
}
