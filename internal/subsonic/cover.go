package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"lxsc/internal/admission"
	"lxsc/internal/httpguard"
)

const maxCoverBytes = 8 << 20
const maxCachedCoverBytes = 256 << 10

type coverImage struct {
	data        []byte
	contentType string
}

type cachedCover struct {
	image   coverImage
	expires time.Time
}

func (s *Server) initCoverClient() {
	s.coverOnce.Do(func() {
		if s.coverHTTP == nil {
			s.coverHTTP = httpguard.NewPublicClient(nil, nil)
		}
		if s.coverLimits == nil {
			s.coverLimits = admission.New(8, 0, 2, 0)
		}
		if s.coverCache == nil {
			// 只缓存不超过 256 KiB 的图片，最多 16 MiB；大图仍可正常返回。
			s.coverCache, _ = lru.New[string, cachedCover](64)
		}
	})
}

func (s *Server) proxyCover(w http.ResponseWriter, r *http.Request, address string) {
	s.coverWithFallback(w, r, address, "", nil)
}

// 配置回退时在服务器读取原图，避免发出 302 后无法观察客户端的图源错误。
func (s *Server) coverWithFallback(w http.ResponseWriter, r *http.Request, address, template string, fields map[string]string) {
	s.initCoverClient()
	customURL := ""
	if template != "" && fields != nil {
		customURL, _ = httpguard.ExpandTemplate(template, fields)
	}
	key := address + "\x00" + customURL
	if cached, ok := s.coverCache.Get(key); ok && time.Now().Before(cached.expires) && r.Context().Err() == nil {
		writeCoverImage(w, r, cached.image)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	release, err := s.coverLimits.Acquire(ctx, currentUser(r).ID)
	if err != nil {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "封面服务繁忙", http.StatusServiceUnavailable)
		return
	}
	defer release()
	var picture coverImage
	err = errors.New("封面地址为空")
	if address != "" {
		primaryCtx := ctx
		stop := func() {}
		if customURL != "" {
			primaryCtx, stop = context.WithTimeout(ctx, 3*time.Second)
		}
		picture, err = s.fetchCover(primaryCtx, address, false)
		stop()
	}
	if err != nil && customURL != "" && ctx.Err() == nil {
		fallbackCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		picture, err = s.fetchCover(fallbackCtx, customURL, true)
		stop()
	}
	if err != nil || ctx.Err() != nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "封面读取失败", http.StatusBadGateway)
		return
	}
	if len(picture.data) <= maxCachedCoverBytes {
		s.coverCache.Add(key, cachedCover{image: picture, expires: time.Now().Add(15 * time.Minute)})
	}
	writeCoverImage(w, r, picture)
}

func (s *Server) fetchCover(ctx context.Context, address string, allowReference bool) (coverImage, error) {
	bad := errors.New("封面接口未返回有效图片")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || !httpguard.ValidTarget(req.URL) {
		return coverImage{}, bad
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.coverHTTP.Do(req)
	if err != nil {
		return coverImage{}, bad
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxCoverBytes {
		return coverImage{}, bad
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil || len(data) > maxCoverBytes {
		return coverImage{}, bad
	}
	// 不信任 Content-Type，不能把 HTML、SVG 或 JSON 错误页当作图片交付。
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "image/x-icon":
		return coverImage{data: data, contentType: contentType}, nil
	}
	if allowReference && len(data) <= 64<<10 {
		// 自定义接口可返回图片 URL；只再读取一次，所有目标与重定向仍须通过公网校验。
		if target := coverReference(data, 0); target != "" {
			return s.fetchCover(ctx, target, false)
		}
	}
	return coverImage{}, bad
}

func coverReference(data []byte, depth int) string {
	if depth > 3 {
		return ""
	}
	value := strings.TrimSpace(string(data))
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return value
	}
	var text string
	if json.Unmarshal(data, &text) == nil {
		return text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil {
		return ""
	}
	for _, key := range []string{"url", "cover", "coverUrl", "pic", "picUrl", "data"} {
		if raw := object[key]; len(raw) > 0 {
			if value := coverReference(raw, depth+1); value != "" {
				return value
			}
		}
	}
	return ""
}

func writeCoverImage(w http.ResponseWriter, r *http.Request, picture coverImage) {
	w.Header().Set("Content-Type", picture.contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=900")
	w.Header().Set("Content-Length", strconv.Itoa(len(picture.data)))
	recordMediaResponse(r, http.StatusOK)
	_, _ = w.Write(picture.data)
}
