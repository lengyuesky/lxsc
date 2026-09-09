package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/music"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

// Server 不复用 Subsonic 或管理员认证，不读取 logbuf。
type Server struct {
	DB         *db.DB
	Auth       *webauth.Manager
	Catalog    *music.Catalog
	Sources    *js.SourceManager
	Settings   *settings.Store
	Version    string
	StartAt    time.Time
	Tokens     *Tokens
	Events     *Events
	client     *http.Client
	concurrent chan struct{}
	probes     chan struct{}
}

func New(database *db.DB, auth *webauth.Manager, catalog *music.Catalog, sources *js.SourceManager, settings *settings.Store, version string, start time.Time) *Server {
	return &Server{DB: database, Auth: auth, Catalog: catalog, Sources: sources, Settings: settings, Version: version, StartAt: start, Tokens: NewTokens(), Events: &Events{}, client: probeClient(), concurrent: make(chan struct{}, 8), probes: make(chan struct{}, 2)}
}
func SensitivePath(path string) bool {
	for _, base := range []string{"/api/debug", "/api/admin/debug-tokens"} {
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}
func SecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for _, key := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods", "Access-Control-Allow-Credentials"} {
		w.Header().Del(key)
	}
}
func output(w http.ResponseWriter, code int, value any) {
	SecurityHeaders(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, code int, value string) {
	output(w, code, map[string]string{"error": value})
}
func busy(w http.ResponseWriter, seconds string) {
	w.Header().Set("Retry-After", seconds)
	failure(w, 429, "rate_limited")
}

// sameOrigin 不信任转发头；HTTPS 反代必须保留/校验 Host。
// 自定义非简单头和关闭 CORS 防止明文后端无法区分外部 scheme 的跨源请求。
func sameOrigin(r *http.Request, required bool) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return !required
	}
	if len(values) != 1 {
		return false
	}
	origin := values[0]
	u, err := url.Parse(origin)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if origin != u.Scheme+"://"+u.Host || strings.ContainsAny(u.Host, "\\ \t\r\n%") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	if r.TLS != nil && u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// objectBody 限长、拒绝重复/未知字段、null、尾随 JSON；不记录原文。
func objectBody(w http.ResponseWriter, r *http.Request, allowed ...string) (map[string]json.RawMessage, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return nil, false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, false
	}
	values := make(map[string]json.RawMessage)
	for d.More() {
		k, err := d.Token()
		key, ok := k.(string)
		if err != nil || !ok || oneOf(key, allowed...) == "" {
			return nil, false
		}
		if _, exists := values[key]; exists {
			return nil, false
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil || string(raw) == "null" {
			return nil, false
		}
		values[key] = raw
	}
	if last, err := d.Token(); err != nil || last != json.Delim('}') {
		return nil, false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, false
	}
	return values, true
}

// limitIO 将正文读取和响应刷新一起纳入期限/撤销边界。
// 敏感 HTTP/1 连接不复用；返回前禁止 net/http 再排空客户端未发送的正文。
func limitIO(w http.ResponseWriter, ctx context.Context) func() {
	controller := http.NewResponseController(w)
	deadline, _ := ctx.Deadline()
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now())
		close(done)
	})
	return func() {
		// 必须在停止撤销监听/释放并发槽之前刷新，不能只等 handler 返回。
		_ = controller.Flush()
		_ = controller.SetReadDeadline(time.Now())
		if !stop() {
			<-done
		}
		// 保留写截止，最终协议收尾也不能无限等待慢客户端。
	}
}

type ioKey struct{}

// SensitiveIO 位于 CORS/管理员认证之前，也覆盖无凭据、方法错误和预检的提前拒绝。
func SensitiveIO(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !SensitivePath(r.URL.Path) || r.Context().Value(ioKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		SecurityHeaders(w)
		if r.ProtoMajor == 1 {
			w.Header().Set("Connection", "close")
			// 不留待 handler 返回后再写 chunk 结束标记；Flush 覆盖全部响应字节。
			w.Header().Set("Transfer-Encoding", "identity")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		defer limitIO(w, ctx)()
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ioKey{}, true)))
	})
}

func (s *Server) ManagementRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(SensitiveIO)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SecurityHeaders(w)
			u := s.Auth.User(r)
			if u == nil {
				failure(w, 401, "需要有效网页登录")
				return
			}
			if !u.IsAdmin {
				failure(w, 403, "仅管理员可访问")
				return
			}
			if r.URL.RawQuery != "" || !sameOrigin(r, r.Method != "GET") {
				failure(w, 403, "同源校验失败")
				return
			}
			if r.Method != "GET" && r.Header.Get("X-LXSC-Debug-Management") != "1" {
				failure(w, 403, "缺少管理请求头")
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		tokens, audit := s.Tokens.List()
		output(w, 200, map[string]any{"tokens": tokens, "audit": audit})
	})
	r.Post("/", s.create)
	r.Delete("/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 {
			failure(w, 400, "撤销不接受请求体")
			return
		}
		if !s.Tokens.Revoke(chi.URLParam(r, "id")) {
			failure(w, 404, "凭据不存在")
			return
		}
		output(w, 200, map[string]bool{"ok": true})
	})
	return r
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	values, ok := objectBody(w, r, "ttlSeconds", "scopes")
	if !ok {
		failure(w, 400, "参数错误")
		return
	}
	seconds := int64(900)
	if raw, exists := values["ttlSeconds"]; exists && json.Unmarshal(raw, &seconds) != nil {
		failure(w, 400, "无效有效期")
		return
	}
	if seconds != 900 && seconds != 3600 && seconds != 21600 && seconds != 86400 {
		failure(w, 400, "有效期仅支持15分钟、1小时、6小时或24小时")
		return
	}
	scopes := []string{"read"}
	if raw, exists := values["scopes"]; exists && json.Unmarshal(raw, &scopes) != nil {
		failure(w, 400, "无效权限")
		return
	}
	if !(len(scopes) == 1 && scopes[0] == "read") && !(len(scopes) == 2 && ((scopes[0] == "read" && scopes[1] == "probe") || (scopes[0] == "probe" && scopes[1] == "read"))) {
		failure(w, 400, "权限必须为read，可额外选择probe")
		return
	}
	u := s.Auth.User(r)
	if u == nil || !u.IsAdmin {
		failure(w, 403, "管理员会话无效")
		return
	}
	view, plain, err := s.Tokens.Create(u.ID, seconds, len(scopes) == 2)
	if err != nil {
		if errors.Is(err, errTokenLimit) {
			busy(w, "60")
		} else {
			failure(w, 503, "凭据创建暂不可用")
		}
		return
	}
	// 再校验创建者，避免与用户降权/删除交错产生可用凭据。
	current, err := s.DB.GetUserByID(r.Context(), u.ID)
	if err != nil || !current.IsAdmin {
		s.Tokens.Revoke(view.ID)
		failure(w, 403, "管理员已失效")
		return
	}
	output(w, 201, map[string]any{"token": plain, "credential": view})
}
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(SensitiveIO)
	r.Use(s.authorize)
	r.Get("/status", s.status)
	r.Get("/events", func(w http.ResponseWriter, r *http.Request) {
		output(w, 200, map[string]any{"events": s.Events.List()})
	})
	r.Post("/probe", s.probe)
	return r
}

type scopeKey struct{}

func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SecurityHeaders(w)
		if !sameOrigin(r, false) {
			failure(w, 403, "cross_origin_denied")
			return
		}
		auth := r.Header.Values("Authorization")
		if r.URL.RawQuery != "" || len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="debug"`)
			failure(w, 401, "invalid_debug_token")
			return
		}
		t, view, code := s.Tokens.acquire(strings.TrimPrefix(auth[0], "Bearer "))
		if code == 429 {
			busy(w, "60")
			return
		}
		if code != 200 {
			failure(w, 401, "invalid_debug_token")
			return
		}
		deadline := time.Now().Add(20 * time.Second)
		if view.ExpiresAt.Before(deadline) {
			deadline = view.ExpiresAt
		}
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		stop := context.AfterFunc(t.ctx, cancel)
		defer stop()
		admitted := false
		defer func() {
			if admitted {
				<-s.concurrent
			}
		}()
		defer limitIO(w, ctx)()
		select {
		case s.concurrent <- struct{}{}:
			admitted = true
		default:
			busy(w, "1")
			return
		}
		if t.ctx.Err() != nil || ctx.Err() != nil {
			failure(w, 401, "invalid_debug_token")
			return
		}
		u, err := s.DB.GetUserByID(ctx, t.owner)
		if ctx.Err() != nil {
			failure(w, 408, "debug_cancelled_or_expired")
			return
		}
		if err != nil || !u.IsAdmin {
			s.Tokens.RevokeOwner(t.owner)
			failure(w, 401, "invalid_debug_token")
			return
		}
		// 除管理接口的即时撤销，还检测其他数据库写入造成的创建者失效。
		// 监听持续到响应刷新完成后 ctx 取消，不能在慢响应仍在途时提前停止。
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					u, err := s.DB.GetUserByID(ctx, t.owner)
					if ctx.Err() != nil {
						return
					}
					if err != nil || !u.IsAdmin {
						s.Tokens.RevokeOwner(t.owner)
						return
					}
				}
			}
		}()
		if t.ctx.Err() != nil {
			failure(w, 401, "invalid_debug_token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, scopeKey{}, view.Scopes)))
	})
}

var safeVersion = regexp.MustCompile(`^[A-Za-z0-9._+\-]{1,100}$`)

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	v := s.Settings.Get()
	version := s.Version
	if !safeVersion.MatchString(version) {
		version = "unknown"
	}
	platforms := []string{}
	health := map[string]int{"total": 0, "ready": 0, "error": 0, "disabled": 0}
	if s.Sources != nil {
		for _, p := range s.Sources.SupportedPlatforms() {
			if Platform(p) != "" {
				platforms = append(platforms, p)
			}
		}
		sources, err := s.DB.ListSources(r.Context())
		if err != nil {
			failure(w, 503, "status_unavailable")
			return
		}
		health["total"] = len(sources)
		for _, source := range sources {
			if !source.Enabled {
				health["disabled"]++
			} else if st := s.Sources.StatusOf(source.ID); st != nil && st.State == "ready" {
				health["ready"]++
			} else {
				health["error"]++
			}
		}
	}
	cacheMode := "timed"
	if v.URLCacheTTL == 0 {
		cacheMode = "disabled"
	}
	if v.URLCacheTTL < 0 {
		cacheMode = "permanent"
	}
	output(w, 200, map[string]any{"version": version, "uptimeSeconds": int64(time.Since(s.StartAt).Seconds()), "streamMode": Mode(v.StreamMode), "urlCacheMode": cacheMode, "platforms": platforms, "sourceHealth": health})
}

var contentRange = regexp.MustCompile(`^bytes [0-9]{1,19}-[0-9]{1,19}/([0-9]{1,19}|\*)$`)

func mediaHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	if value, _, err := mime.ParseMediaType(h.Get("Content-Type")); err == nil && oneOf(value, "audio/mpeg", "audio/mp4", "audio/flac", "audio/x-flac", "audio/ogg", "audio/wav", "audio/x-wav", "application/octet-stream", "application/ogg") != "" {
		out["contentType"] = value
	}
	if value := h.Get("Content-Length"); len(value) <= 19 {
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
			out["contentLength"] = strconv.FormatInt(n, 10)
		}
	}
	if value := h.Get("Content-Range"); contentRange.MatchString(value) {
		out["contentRange"] = value
	}
	if value := h.Get("Accept-Ranges"); value == "bytes" || value == "none" {
		out["acceptRanges"] = value
	}
	return out
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	scopes, _ := r.Context().Value(scopeKey{}).([]string)
	if len(scopes) != 2 {
		failure(w, 403, "probe_scope_required")
		return
	}
	values, ok := objectBody(w, r, "trackId", "quality")
	if r.Context().Err() != nil {
		failure(w, 408, "probe_cancelled_or_expired")
		return
	}
	var id, quality string
	if !ok || json.Unmarshal(values["trackId"], &id) != nil || !ValidTrackID(id) || json.Unmarshal(values["quality"], &quality) != nil || Quality(quality) == "" || music.QualityRank(quality) == 0 {
		failure(w, 400, "invalid_probe_request")
		return
	}
	select {
	case s.probes <- struct{}{}:
		defer func() { <-s.probes }()
	default:
		busy(w, "1")
		return
	}
	events := make([]Event, 0, 3)
	headers := map[string]string{}
	stage := func(name string, started time.Time, cached bool, status int, err error) {
		if status < 100 || status > 599 {
			status = 0
		}
		p, _ := music.ParseID(id)
		e := Event{Time: time.Now().UTC(), Stage: name, TrackID: id, Platform: p.Source, Quality: quality, Mode: Mode(s.Settings.Get().StreamMode), Cached: cached, Status: status, Error: ErrorCode(err), ElapsedMS: time.Since(started).Milliseconds()}
		events = append(events, e)
		s.Events.Add(e)
	}
	started := time.Now()
	in, err := s.Catalog.Track(r.Context(), id)
	stage("metadata", started, false, 0, err)
	if err == nil {
		started = time.Now()
		var resolution music.URLResolution
		resolution, err = s.Catalog.ResolvePlaybackURL(r.Context(), in, quality)
		stage("resolve", started, resolution.Cached, 0, err)
		if err == nil {
			started = time.Now()
			var req *http.Request
			req, err = http.NewRequestWithContext(r.Context(), http.MethodGet, resolution.Result.URL, nil)
			status := 0
			if err == nil {
				req.Header.Set("Range", "bytes=0-0")
				req.Header.Set("Accept-Encoding", "identity")
				req.Header.Set("User-Agent", "lxsc-debug-probe")
				var resp *http.Response
				resp, err = s.client.Do(req)
				if err == nil {
					status = resp.StatusCode
					headers = mediaHeaders(resp.Header)
					_ = resp.Body.Close()
				}
			}
			stage("probe", started, resolution.Cached, status, err)
		}
	}
	if r.Context().Err() != nil {
		failure(w, 408, "probe_cancelled_or_expired")
		return
	}
	output(w, 200, map[string]any{"events": events, "mediaHeaders": headers, "perspective": "server_only", "cacheSideEffects": true})
}
