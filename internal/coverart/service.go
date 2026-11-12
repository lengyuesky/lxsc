// Package coverart shares bounded, public-only cover delivery across music protocols.
package coverart

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
)

const maxCoverBytes = 8 << 20

// RequestTimeout includes metadata resolution when the caller sets an earlier deadline.
const RequestTimeout = coverRequestTimeout

// Service owns shared work, cache and delivery limits, not authentication.
// Configure dependencies before serving requests; the zero value is usable.
type Service struct {
	HTTP     *http.Client
	Limits   *admission.Gate
	Delivery *admission.Gate

	once  sync.Once
	cache *coverWork
}

// Resource must be resolved and authorized again by the protocol on every request.
// Non-nil Fields permit temporary-image fallback, but never bypass unsafe-target errors.
type Resource struct {
	UserID   int64
	Address  string
	Template string
	Fields   map[string]string
}

type coverImage struct {
	data        []byte
	contentType string
	origin      string
}

func (s *Service) init() {
	s.once.Do(func() {
		if s.HTTP == nil {
			s.HTTP = httpguard.NewPublicClient(nil, nil)
		}
		if s.Limits == nil {
			s.Limits = admission.New(coverConcurrent, coverQueueLimit, coverUserConcurrent, coverUserQueueLimit)
		}
		if s.Delivery == nil {
			// 上游合并后仍要单独限制慢客户端持有的大图，不能只限制下载阶段。
			s.Delivery = admission.New(16, 0, 0, 0)
		}
		if s.cache == nil {
			s.cache = newCoverWork()
		}
	})
}

// AcquireDelivery 供已鉴权的本地图片交付复用大图并发限制；必须在读取图片前调用。
func (s *Service) AcquireDelivery(ctx context.Context) (func(), error) {
	s.init()
	return s.Delivery.Acquire(ctx, 0)
}

// AcquireWork 供上传图片的有界解析复用全站与用户并发限制。
func (s *Service) AcquireWork(ctx context.Context, userID int64) (func(), error) {
	s.init()
	return s.Limits.Acquire(ctx, userID)
}

// Serve reads and validates images; it never forwards client headers or redirects to an upstream.
func (s *Service) Serve(w http.ResponseWriter, r *http.Request, resource Resource, event *diagnostics.Event) {
	ctx, cancel := context.WithTimeout(r.Context(), coverRequestTimeout)
	defer cancel()
	s.serveCover(ctx, w, r, resource, event)
}

func (s *Service) serveCover(ctx context.Context, w http.ResponseWriter, r *http.Request, resource Resource, event *diagnostics.Event) {
	s.init()
	customURL := ""
	if resource.Template != "" && resource.Fields != nil {
		customURL, _ = httpguard.ExpandTemplate(resource.Template, resource.Fields)
	}
	key := resource.Address + "\x00" + customURL
	picture, cached, err := s.cache.load(ctx, key, resource.UserID, s.Limits, func(workCtx context.Context) (coverImage, error) {
		return s.loadCover(workCtx, resource.Address, customURL)
	})
	if r.Context().Err() != nil {
		return
	}
	if err == nil {
		s.deliverCover(w, r, picture, cached, nil, resource.Fields != nil, event)
		return
	}
	safeFailure := !errors.Is(err, httpguard.ErrUnsafeTarget) && !errors.Is(err, httpguard.ErrRedirectLimit)
	if safeFailure {
		if old, ok := s.cache.staleImage(key); ok {
			old.origin = "stale"
			s.deliverCover(w, r, old, true, err, resource.Fields != nil, event)
			return
		}
	}
	// 仅对已经解析且通过权限检查的资源兜底，鉴权、未知资源和不安全目标仍保留错误。
	if resource.Fields != nil && safeFailure {
		if placeholder, placeholderErr := placeholderCoverImage(); placeholderErr == nil {
			s.deliverCover(w, r, placeholder, false, err, true, event)
			return
		}
	}
	RecordOutcome(event, "", false, err)
	w.Header().Set("Cache-Control", "no-store")
	if errors.Is(err, admission.ErrBusy) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "封面服务繁忙", http.StatusServiceUnavailable)
	} else {
		http.Error(w, "封面读取失败", http.StatusBadGateway)
	}
}

func (s *Service) deliverCover(w http.ResponseWriter, r *http.Request, picture coverImage, cached bool, cause error, allowPlaceholder bool, event *diagnostics.Event) {
	if picture.origin != "placeholder" {
		release, err := s.Delivery.Acquire(r.Context(), 0)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			if allowPlaceholder {
				if fallback, fallbackErr := placeholderCoverImage(); fallbackErr == nil {
					s.deliverCover(w, r, fallback, false, err, false, event)
					return
				}
			}
			RecordOutcome(event, "", false, err)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "封面服务繁忙", http.StatusServiceUnavailable)
			return
		}
		defer release()
	} else {
		s.cache.placeholder()
	}
	RecordOutcome(event, picture.origin, cached, cause)
	writeCoverImage(w, r, picture)
}

func (s *Service) loadCover(ctx context.Context, address, customURL string) (coverImage, error) {
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

func (s *Service) fetchCover(ctx context.Context, address string, allowReference bool) (coverImage, error) {
	bad := errors.New("封面接口未返回有效图片")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil || !httpguard.ValidTarget(req.URL) {
		return coverImage{}, httpguard.ErrUnsafeTarget
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.HTTP.Do(req)
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
