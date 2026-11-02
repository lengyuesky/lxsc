// Package fnmusic 提供独立的飞牛音乐客户端协议适配，不依赖 fnOS。
package fnmusic

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/authlimit"
	"lxsc/internal/coverart"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
	"lxsc/internal/mediastream"
	"lxsc/internal/music"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

// Server 使用同一套 lxsc 用户、音乐核心和数据库，认证会话与 Web 协议隔离。
type Server struct {
	DB       *db.DB
	Catalog  *music.Catalog
	Settings *settings.Store
	Auth     *webauth.Manager
	Version  string
	GUID     string

	Media       *mediastream.Service
	Covers      *coverart.Service
	Diagnostics *diagnostics.Events

	sessions sessionStore
	searches searchStore
	limits   authlimit.Limiter
}

const compatibilityVersion = "0.1.0"

// New 初始化稳定的服务器标识；不创建用户，也不修改已有音乐数据。
func New(ctx context.Context, database *db.DB, catalog *music.Catalog, settings *settings.Store, auth *webauth.Manager, version string) (*Server, error) {
	guid := database.GetSetting(ctx, "fnmusic.serverGUID", "")
	if !validGUID(guid) {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		guid = hex.EncodeToString(raw[:])
		if err := database.SetSetting(ctx, "fnmusic.serverGUID", guid); err != nil {
			return nil, err
		}
	}
	return &Server{DB: database, Catalog: catalog, Settings: settings, Auth: auth, Version: version, GUID: guid, Covers: &coverart.Service{}}, nil
}

// Probe 是直连连通性探测，不冒充云中继或真实 NAS 系统身份。
func (s *Server) Probe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, s.GUID)
}

// Routes 挂载到 /music；只实现有明确边界的音乐接口。
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/api/v1/initialization/state", func(w http.ResponseWriter, r *http.Request) {
		success(w, map[string]any{"initialized": true})
	})
	r.Get("/api/v1/sys/config", s.config)
	r.Post("/api/v1/user/password-login", s.login)
	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)
		r.Get("/api/v1/user/me", s.me)
		r.Get("/api/v1/sys/info", s.config)
		r.Get("/api/v1/settings/server", s.serverSettings)
		r.Post("/api/v1/user/logout", s.logout)
		r.Get("/api/v1/shared-library/list", s.libraries)
		r.Get("/api/v1/task/list", emptyList)
		r.Get("/api/v1/genre/list", emptyList)
		r.Get("/api/v1/playlist/list", s.playlists)
		r.Get("/api/v1/playlist/detail", s.playlistDetail)
		r.Get("/api/v1/playlist/batch-detail", s.playlistBatch)
		r.Get("/api/v1/track/playlist-detail/list", s.playlistTracks)
		r.Get("/api/v1/track/list", s.tracks)
		r.Get("/api/v1/track/metadata", s.metadata)
		r.Get("/api/v1/track/audio-info", s.metadata)
		r.Get("/api/v1/favorite-track/list", s.favorites)
		r.Get("/api/v1/play-history/list", s.history)
		r.Get("/api/v1/album/list", s.albums)
		r.Get("/api/v1/artist/list", s.artists)
		r.Get("/api/v1/album/detail", s.albumDetail)
		r.Get("/api/v1/artist/detail", s.artistDetail)
		r.Get("/api/v1/track/album-detail/list", s.tracks)
		r.Get("/api/v1/track/artist-detail/list", s.tracks)
		r.Get("/api/v1/album/artist-detail/list", s.albums)
		r.Get("/api/v1/search/track", s.searchTracks)
		r.Get("/api/v1/search/album", s.searchAlbums)
		r.Get("/api/v1/search/artist", s.searchArtists)
		r.Get("/api/v1/search/playlist", s.searchPlaylists)
		r.Get("/api/v1/search/suggest", s.searchSuggest)
		r.Get("/api/v1/lyric/list", s.lyrics)
		r.Post("/api/v1/event/report", s.events)
		r.Get("/api/v1/track/stream", s.stream)
		r.Head("/api/v1/track/stream", s.stream)
		r.Get("/api/v1/track/stream/{guid}", s.stream)
		r.Head("/api/v1/track/stream/{guid}", s.stream)
		r.Get("/static/cover", s.cover)
		r.Get("/api/v1/static/cover", s.cover)
		r.Get("/api/v1/track/transcode", unsupported)
		r.Post("/api/v1/track/transcode", unsupported)
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		failure(w, http.StatusNotFound, "未实现的飞牛音乐接口")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		failure(w, http.StatusMethodNotAllowed, "不支持的请求方法")
	})
	return r
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	success(w, map[string]any{
		"serverGUID": s.GUID, "serverName": "lxsc", "serverVersion": compatibilityVersion,
		"mediasrvVersion": compatibilityVersion, "nasOAuth": false, "lxscVersion": s.Version,
	})
}

func (s *Server) serverSettings(w http.ResponseWriter, r *http.Request) {
	success(w, map[string]any{"name": "lxsc", "lang": "zh-CN"})
}

func response(w http.ResponseWriter, status, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data any    `json:"data"`
	}{code, message, data})
}

func success(w http.ResponseWriter, data any) { response(w, http.StatusOK, 0, "ok", data) }
func failure(w http.ResponseWriter, status int, message string) {
	response(w, status, status, message, nil)
}

func emptyList(w http.ResponseWriter, r *http.Request) {
	success(w, map[string]any{"list": []any{}, "total": 0, "sort": ""})
}

func unsupported(w http.ResponseWriter, r *http.Request) {
	failure(w, http.StatusNotImplemented, "本服务不支持音频转码或 HLS")
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		failure(w, http.StatusUnsupportedMediaType, "只接收 JSON 请求")
		return false
	}
	origin := "http://" + r.Host
	if httpguard.Secure(r) {
		origin = "https://" + r.Host
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (len(origins) == 1 && !strings.EqualFold(origins[0], origin)) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		failure(w, http.StatusForbidden, "拒绝跨站音乐请求")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := decoder.Decode(target); err != nil {
		failure(w, http.StatusBadRequest, "请求体必须是 JSON")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		failure(w, http.StatusBadRequest, "请求体只能包含一个 JSON 对象")
		return false
	}
	return true
}

func (s *Server) userView(u *db.User) map[string]any {
	role := "member"
	if u.IsAdmin {
		role = "admin"
	}
	return map[string]any{
		"guid": identifier("user", strconv.FormatInt(u.ID, 10)), "name": u.Name, "role": role,
		"createdAt": u.CreatedAt, "updatedAt": u.CreatedAt, "lastAccessedAt": time.Now().Unix(),
	}
}
