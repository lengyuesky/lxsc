package subsonic

import (
	"net/http"
	"strconv"
	"strings"

	"lxsc/internal/music"
)

// writeMediaHead 只报告已经验证的音频元数据；实际 GET 仍使用原播放方式。
func (s *Server) writeMediaHead(w http.ResponseWriter, r *http.Request, resolution music.URLResolution, resp *http.Response) {
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

// 最小 Range 的 Content-Length 是探测片段长度，必须从 Content-Range 取完整大小。
// 未提供或不合法时保持未知，不能把 1 字节或估算的歌曲大小报告为完整音频长度。
func fullMediaLength(resp *http.Response) (int64, bool) {
	if resp.StatusCode == http.StatusOK {
		length, err := strconv.ParseUint(resp.Header.Get("Content-Length"), 10, 63)
		return int64(length), err == nil
	}
	if resp.StatusCode != http.StatusPartialContent {
		return 0, false
	}
	value, ok := strings.CutPrefix(resp.Header.Get("Content-Range"), "bytes ")
	if !ok {
		return 0, false
	}
	span, total, ok := strings.Cut(value, "/")
	if !ok {
		return 0, false
	}
	first, last, ok := strings.Cut(span, "-")
	if !ok {
		return 0, false
	}
	start, startErr := strconv.ParseUint(first, 10, 63)
	end, endErr := strconv.ParseUint(last, 10, 63)
	length, lengthErr := strconv.ParseUint(total, 10, 63)
	return int64(length), startErr == nil && endErr == nil && lengthErr == nil && end >= start && length > end
}
