package fnmusic

import (
	"mime"
	"net/http"
	"strings"
	"unicode"

	"lxsc/internal/mediastream"
)

type downloadWriter struct {
	http.ResponseWriter
	title string
}

func (w *downloadWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *downloadWriter) WriteHeader(status int) {
	if status == http.StatusOK || status == http.StatusPartialContent {
		name := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || r == '/' || r == '\\' {
				return '_'
			}
			return r
		}, w.title)
		if name == "" {
			name = "download"
		}
		// 扩展名依据实际交付类型，不能用用户请求的音质猜测。
		contentType, _, _ := mime.ParseMediaType(w.Header().Get("Content-Type"))
		switch contentType {
		case "audio/wav", "audio/x-wav", "audio/wave":
			name += ".wav"
		case "audio/flac", "audio/x-flac":
			name += ".flac"
		case "audio/mpeg":
			name += ".mp3"
		case "audio/mp4":
			name += ".m4a"
		case "audio/ogg":
			name += ".ogg"
		}
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	}
	w.ResponseWriter.WriteHeader(status)
}

func (s *Server) downloadDetail(w http.ResponseWriter, r *http.Request) {
	request := r.Clone(r.Context())
	query := request.URL.Query()
	query.Set("guid", query.Get("trackGUID"))
	request.URL.RawQuery = query.Encode()
	s.metadata(w, request)
}

func binaryDownload(r *http.Request) bool {
	return r.URL.Path == "/music/api/v1/download/track"
}

// downloadFailure 中止文件响应，避免原版下载器把非音频错误正文保存成已下载文件。
// 不刷出错误正文；net/http 用 ErrAbortHandler 结束当前流，客户端得到传输失败。
func downloadFailure(w http.ResponseWriter, r *http.Request, status int) {
	mediastream.RecordResponse(r, status)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	panic(http.ErrAbortHandler)
}

func writeDownloadError(w http.ResponseWriter, r *http.Request, code int, _ string) {
	status := http.StatusBadGateway
	switch code {
	case mediastream.ErrBusy:
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "2")
	case mediastream.ErrWrongAuth:
		status = http.StatusUnauthorized
	case mediastream.ErrMissingParam:
		status = http.StatusBadRequest
	case mediastream.ErrNotFound:
		status = http.StatusNotFound
	}
	downloadFailure(w, r, status)
}

// 原版原始下载完成后会清理空转码任务；本服务没有创建服务端转码文件。
func (s *Server) deleteDownloadTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DownloadID string `json:"downloadId"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.DownloadID != "" {
		failure(w, http.StatusNotFound, "下载任务不存在")
		return
	}
	success(w, map[string]any{"deleted": true})
}
