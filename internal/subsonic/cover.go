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

	"lxsc/internal/admission"
	"lxsc/internal/httpguard"
)

const maxCoverBytes = 8 << 20

type coverImage struct {
	data        []byte
	contentType string
	origin      string
}

func (s *Server) initCoverClient() {
	s.coverOnce.Do(func() {
		if s.coverHTTP == nil {
			s.coverHTTP = httpguard.NewPublicClient(nil, nil)
		}
		if s.coverLimits == nil {
			s.coverLimits = admission.New(coverConcurrent, coverQueueLimit, coverUserConcurrent, coverUserQueueLimit)
		}
		if s.coverDelivery == nil {
			// 上游合并后仍要单独限制慢客户端持有的大图，不能只限制下载阶段。
			s.coverDelivery = admission.New(16, 0, 0, 0)
		}
		if s.coverCache == nil {
			s.coverCache = newCoverWork()
		}
	})
}

func (s *Server) proxyCover(w http.ResponseWriter, r *http.Request, address string) {
	s.coverWithFallback(w, r, address, "", nil)
}

// 配置回退时在服务器读取原图，避免发出 302 后无法观察客户端的图源错误。
func (s *Server) coverWithFallback(w http.ResponseWriter, r *http.Request, address, template string, fields map[string]string) {
	ctx, cancel := context.WithTimeout(r.Context(), coverRequestTimeout)
	defer cancel()
	s.serveCover(ctx, w, r, address, template, fields)
}

func (s *Server) serveCover(ctx context.Context, w http.ResponseWriter, r *http.Request, address, template string, fields map[string]string) {
	s.initCoverClient()
	customURL := ""
	if template != "" && fields != nil {
		customURL, _ = httpguard.ExpandTemplate(template, fields)
	}
	key := address + "\x00" + customURL
	picture, cached, err := s.coverCache.load(ctx, key, currentUser(r).ID, s.coverLimits, func(workCtx context.Context) (coverImage, error) {
		return s.loadCover(workCtx, address, customURL)
	})
	if r.Context().Err() != nil {
		return
	}
	if err == nil {
		s.deliverCover(w, r, picture, cached, nil, fields != nil)
		return
	}
	if old, ok := s.coverCache.staleImage(key); ok {
		old.origin = "stale"
		s.deliverCover(w, r, old, true, err, fields != nil)
		return
	}
	// 仅对已经解析且通过权限检查的资源兜底，鉴权、未知资源和不安全目标仍保留错误。
	if fields != nil && !errors.Is(err, httpguard.ErrUnsafeTarget) && !errors.Is(err, httpguard.ErrRedirectLimit) {
		if placeholder, placeholderErr := placeholderCoverImage(); placeholderErr == nil {
			s.deliverCover(w, r, placeholder, false, err, true)
			return
		}
	}
	recordCoverOutcome(r, "", false, err)
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, admission.ErrBusy) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "封面服务繁忙", http.StatusServiceUnavailable)
	} else {
		http.Error(w, "封面读取失败", http.StatusBadGateway)
	}
}

func (s *Server) deliverCover(w http.ResponseWriter, r *http.Request, picture coverImage, cached bool, cause error, allowPlaceholder bool) {
	if picture.origin != "placeholder" {
		release, err := s.coverDelivery.Acquire(r.Context(), 0)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			if allowPlaceholder {
				if fallback, fallbackErr := placeholderCoverImage(); fallbackErr == nil {
					s.deliverCover(w, r, fallback, false, err, false)
					return
				}
			}
			recordCoverOutcome(r, "", false, err)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "封面服务繁忙", http.StatusServiceUnavailable)
			return
		}
		defer release()
	} else {
		s.coverCache.placeholder()
	}
	recordCoverOutcome(r, picture.origin, cached, cause)
	writeCoverImage(w, r, picture)
}

func (s *Server) loadCover(ctx context.Context, address, customURL string) (coverImage, error) {
	var picture coverImage
	err := errors.New("封面地址为空")
	if address != "" {
		primaryCtx := ctx
		stop := func() {}
		if customURL != "" {
			budget := 3 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				// 排队占用的时间不能挤掉备用接口的等待预算。
				budget = min(budget, max(250*time.Millisecond, time.Until(deadline)-5*time.Second))
			}
			primaryCtx, stop = context.WithTimeout(ctx, budget)
		}
		picture, err = s.fetchCover(primaryCtx, address, false)
		stop()
		picture.origin = "original"
	}
	if err != nil && customURL != "" && ctx.Err() == nil {
		fallbackCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		picture, err = s.fetchCover(fallbackCtx, customURL, true)
		stop()
		picture.origin = "custom"
	}
	return picture, err
}

func (s *Server) fetchCover(ctx context.Context, address string, allowReference bool) (coverImage, error) {
	bad := errors.New("封面接口未返回有效图片")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || !httpguard.ValidTarget(req.URL) {
		return coverImage{}, httpguard.ErrUnsafeTarget
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.coverHTTP.Do(req)
	if err != nil {
		return coverImage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxCoverBytes {
		return coverImage{}, bad
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCoverBytes+1))
	if err != nil || len(data) > maxCoverBytes {
		if ctx.Err() != nil {
			return coverImage{}, ctx.Err()
		}
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
	if picture.origin == "placeholder" {
		w.Header().Set("Cache-Control", "private, no-store, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
	} else if picture.origin == "stale" {
		w.Header().Set("Cache-Control", "private, no-cache, max-age=0")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(picture.data)))
	_, _ = w.Write(picture.data)
	// 归还大图传输名额前刷出最后一段；诊断包装器会保留刷新失败。
	_ = http.NewResponseController(w).Flush()
}
