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

// defaultCoverMaxAge 适用于无法确认图片地址与封面标识绑定的真实封面。
const defaultCoverMaxAge = 15 * time.Minute

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
	// MaxEdge 请求不超过该边长的缩略图，仅对支持按需缩放的图床生效；0 表示原图。
	MaxEdge int
	// MaxAge 延长原图的客户端缓存时长；仅当协议层确认封面标识随图片地址变化时设置。
	MaxAge time.Duration
}

type coverImage struct {
	data        []byte
	contentType string
	origin      string
	etag        string
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
	address := sizedAddress(resource.Address, resource.MaxEdge)
	key, primary := address+"\x00"+customURL, ""
	if address != "" {
		// 原图只取决于地址；备用接口的结果仍按各自的匹配字段分开缓存。
		primary = address + "\x00"
	}
	picture, cached, err := s.cache.load(ctx, key, primary, resource.UserID, s.Limits, func(workCtx context.Context) (coverImage, error) {
		return s.loadCover(workCtx, address, resource.Address, customURL)
	})
	if r.Context().Err() != nil {
		return
	}
	if err == nil {
		s.deliverCover(w, r, picture, cached, nil, resource.Fields != nil, resource.MaxAge, event)
		return
	}
	safeFailure := !unsafeFailure(err)
	if safeFailure {
		if old, ok := s.cache.staleImage(key, primary); ok {
			old.origin = "stale"
			s.deliverCover(w, r, old, true, err, resource.Fields != nil, 0, event)
			return
		}
	}
	// 仅对已经解析且通过权限检查的资源兜底，鉴权、未知资源和不安全目标仍保留错误。
	if resource.Fields != nil && safeFailure {
		if placeholder, placeholderErr := placeholderCoverImage(); placeholderErr == nil {
			s.deliverCover(w, r, placeholder, false, err, true, 0, event)
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

func unsafeFailure(err error) bool {
	return errors.Is(err, httpguard.ErrUnsafeTarget) || errors.Is(err, httpguard.ErrRedirectLimit)
}

func (s *Service) deliverCover(w http.ResponseWriter, r *http.Request, picture coverImage, cached bool, cause error, allowPlaceholder bool, maxAge time.Duration, event *diagnostics.Event) {
	if picture.origin != "placeholder" {
		release, err := s.Delivery.Acquire(r.Context(), 0)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			if allowPlaceholder {
				if fallback, fallbackErr := placeholderCoverImage(); fallbackErr == nil {
					s.deliverCover(w, r, fallback, false, err, false, 0, event)
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
	writeCoverImage(w, r, picture, maxAge)
}

// loadCover 依次尝试 address、未缩放的 original 和备用接口；address 为空时只用备用接口。
func (s *Service) loadCover(ctx context.Context, address, original, customURL string) (coverImage, error) {
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
		if err != nil && original != address && !unsafeFailure(err) && primaryCtx.Err() == nil {
			// 缩略图由图床按需生成，偶发失败时退回未缩放的原图，不直接降级为备用图或默认图。
			picture, err = s.fetchCover(primaryCtx, original, false)
		}
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
		return coverImage{data: data, contentType: contentType, etag: coverETag(data)}, nil
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

func writeCoverImage(w http.ResponseWriter, r *http.Request, picture coverImage, maxAge time.Duration) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	switch picture.origin {
	case "placeholder":
		w.Header().Set("Cache-Control", "private, no-store, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
	case "stale":
		w.Header().Set("Cache-Control", "private, no-cache, max-age=0")
	default:
		// 备用接口按歌曲文字匹配，结果可能随原图恢复而变化，不延长缓存。
		if picture.origin != "original" || maxAge < defaultCoverMaxAge {
			maxAge = defaultCoverMaxAge
		}
		w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(int(maxAge/time.Second)))
	}
	// 默认图在本地生成，不带校验值，始终完整返回。
	if picture.etag != "" {
		w.Header().Set("ETag", picture.etag)
		if notModified(r, picture.etag) {
			// 客户端缓存到期后只需重新确认，不再重复下载图片正文。
			w.WriteHeader(http.StatusNotModified)
			_ = http.NewResponseController(w).Flush()
			return
		}
	}
	w.Header().Set("Content-Type", picture.contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(picture.data)))
	_, _ = w.Write(picture.data)
	// 归还大图传输名额前刷出最后一段；诊断包装器会保留刷新失败。
	_ = http.NewResponseController(w).Flush()
}
