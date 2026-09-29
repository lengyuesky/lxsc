package subsonic

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/httpguard"
)

const maxCoverBytes = 8 << 20

func (s *Server) proxyCover(w http.ResponseWriter, r *http.Request, address string) {
	s.coverOnce.Do(func() {
		if s.coverHTTP == nil {
			s.coverHTTP = httpguard.NewPublicClient(nil, nil)
		}
		s.coverLimits = admission.New(8, 0, 2, 0)
	})
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	release, err := s.coverLimits.Acquire(ctx, currentUser(r).ID)
	if err != nil {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "封面服务繁忙", http.StatusServiceUnavailable)
		return
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || !httpguard.ValidTarget(req.URL) {
		http.Error(w, "封面地址不受支持", http.StatusBadGateway)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.coverHTTP.Do(req)
	if err != nil {
		http.Error(w, "封面请求失败", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxCoverBytes {
		http.Error(w, "封面响应无效或过大", http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil || len(data) > maxCoverBytes {
		http.Error(w, "封面读取失败或过大", http.StatusBadGateway)
		return
	}
	// 不信任上游 Content-Type，也不允许 SVG/HTML 等主动内容以本站来源执行。
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "image/x-icon":
	default:
		http.Error(w, "仅支持栅格图片封面", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}
