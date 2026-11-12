package mediastream

import (
	"net/http"
	"strconv"
	"strings"

	"lxsc/internal/httpguard"
	"lxsc/internal/music"
)

// writeMediaHead 只报告已经验证的音频元数据；实际 GET 仍使用原播放方式。
func (s *Service) WriteHead(w http.ResponseWriter, r *http.Request, resolution music.URLResolution, resp *http.Response) {
	if r.Context().Err() != nil {
		return
	}
	for _, name := range []string{"Content-Type", "Accept-Ranges", "Last-Modified", "ETag"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if value := strings.ToLower(w.Header().Get("Content-Type")); value == "" || strings.HasPrefix(value, "application/octet") {
		_, _, contentType := music.QualityMeta(resolution.Result.Quality)
		w.Header().Set("Content-Type", contentType)
	}
	if length, ok := fullMediaLength(resp); ok {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		// 有效的 206 已证明支持字节范围；部分 CDN 不另发 Accept-Ranges。
		if resp.StatusCode == http.StatusPartialContent && w.Header().Get("Accept-Ranges") == "" {
			w.Header().Set("Accept-Ranges", "bytes")
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	recordMediaResponse(r, http.StatusOK)
	w.WriteHeader(http.StatusOK)
}

// 最小 Range 的 Content-Length 是探测片段长度，完整大小必须取自 Content-Range。
func fullMediaLength(resp *http.Response) (int64, bool) { return httpguard.MediaLength(resp) }
