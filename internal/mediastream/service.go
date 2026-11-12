// Package mediastream 统一提供已认证音乐请求的取链、媒体预检和交付。
package mediastream

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
	"lxsc/internal/playback"
	"lxsc/internal/settings"
)

// Service 不管理客户端认证或歌单权限，各协议入口必须先完成访问校验。
type Service struct {
	Catalog     *music.Catalog
	Settings    *settings.Store
	HTTP        *http.Client
	Log         *slog.Logger
	Diagnostics *diagnostics.Events
}

type ErrorWriter func(http.ResponseWriter, *http.Request, int, string)

const (
	ErrGeneric      = 0
	ErrMissingParam = 10
	ErrWrongAuth    = 40
	ErrNotFound     = 70
	ErrBusy         = -1

	PrepareLimit = prepareLimit
	QualityGrace = qualityGrace
)

type userKey struct{}
type clientEventKey struct{}
type abortTransferKey struct{}
type headerProbeKey struct{}
type mediaDescriptionKey struct{}

// MediaDescription 只包含实际验证响应的文件属性，不包含媒体地址或认证值。
type MediaDescription struct {
	ContentType    string
	Length         int64
	KnownLength    bool
	ETag           string
	LastModified   string
	ResourceDigest [32]byte
}

// ObserveMediaDescription 允许协议适配在交付前记录已选择的音频表示。
func ObserveMediaDescription(r *http.Request, observe func(MediaDescription) error) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), mediaDescriptionKey{}, observe))
}

// HeaderProbe 仅供经过授权的调试入口使用；不会读取音频、发布验证缓存或增加媒体成功评分。
// 普通播放器不能用查询参数选择这个模式。
func HeaderProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, headerProbeKey{}, true)
}

func headersOnly(ctx context.Context) bool {
	value, _ := ctx.Value(headerProbeKey{}).(bool)
	return value
}

// Bind 只供已认证的协议入口使用，共享原请求取消及诊断生命周期。
func Bind(r *http.Request, user *db.User, event *diagnostics.Event) *http.Request {
	ctx := context.WithValue(r.Context(), userKey{}, user)
	if event != nil {
		ctx = context.WithValue(ctx, clientEventKey{}, event)
	}
	return r.WithContext(ctx)
}

func currentUser(r *http.Request) *db.User {
	u, _ := r.Context().Value(userKey{}).(*db.User)
	return u
}

func param(r *http.Request, name string) string {
	if value := r.URL.Query().Get(name); value != "" {
		return value
	}
	if r.Method == http.MethodPost {
		return r.PostFormValue(name)
	}
	return ""
}

func paramInt(r *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(param(r, name))
	if err != nil {
		return fallback
	}
	return value
}

func recordMediaResponse(r *http.Request, status int) {
	playback.FromContext(r.Context()).Response(status)
	event, _ := r.Context().Value(clientEventKey{}).(*diagnostics.Event)
	if event != nil {
		event.Status, event.Format, event.Result = status, "binary", "ok"
		if status >= http.StatusBadRequest {
			event.Result = "failed"
		}
	}
}

// RecordResponse 允许协议入口把媒体错误状态关联到相同诊断请求。
func RecordResponse(r *http.Request, status int) { recordMediaResponse(r, status) }

// ServeTrack 限定客户端可影响的媒体参数，不允许覆盖管理员播放模式。
func (s *Service) ServeTrack(w http.ResponseWriter, r *http.Request, user *db.User, id, quality string, fail ErrorWriter) {
	s.serveSelectedTrack(w, r, user, id, quality, true, fail)
}

// ServeDownloadTrack 使用同一交付策略，但下载不保存播放元数据或计作收听。
func (s *Service) ServeDownloadTrack(w http.ResponseWriter, r *http.Request, user *db.User, id string, fail ErrorWriter) {
	s.serveSelectedTrack(w, r, user, id, "", false, fail)
}

func (s *Service) serveSelectedTrack(w http.ResponseWriter, r *http.Request, user *db.User, id, quality string, persist bool, fail ErrorWriter) {
	request := r.Clone(r.Context())
	query := url.Values{"id": {id}}
	if quality != "" {
		query.Set("quality", quality)
	}
	request.URL.RawQuery = query.Encode()
	request.Form, request.PostForm = nil, nil
	if !persist {
		request = request.WithContext(context.WithValue(request.Context(), abortTransferKey{}, true))
	}
	s.Serve(w, Bind(request, user, nil), persist, fail)
}
