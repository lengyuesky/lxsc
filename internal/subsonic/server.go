package subsonic

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	lru "github.com/hashicorp/golang-lru/v2"

	"lxsc/internal/admission"
	"lxsc/internal/authlimit"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/music"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
)

// Server Subsonic API 服务
type Server struct {
	Diagnostics *diagnostics.Events
	DB          *db.DB
	Catalog     *music.Catalog
	Settings    *settings.Store
	Secret      *secret.Box
	Log         *slog.Logger
	HTTP        *http.Client // 代理拉流用
	authLimits  authlimit.Limiter
	coverOnce   sync.Once
	coverHTTP   *http.Client
	coverLimits *admission.Gate
	coverCache  *lru.Cache[string, cachedCover]
}

type handlerFunc func(w http.ResponseWriter, r *http.Request)

// Routes 挂载到 /rest
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	handlers := map[string]handlerFunc{
		"ping":                      s.ping,
		"getLicense":                s.getLicense,
		"getOpenSubsonicExtensions": s.getOpenSubsonicExtensions,
		"getUser":                   s.getUser,
		"getUsers":                  s.getUsers,
		"getScanStatus":             s.getScanStatus,
		"startScan":                 s.getScanStatus,
		"getMusicFolders":           s.getMusicFolders,
		"getIndexes":                s.getIndexes,
		"getArtists":                s.getArtists,
		"getMusicDirectory":         s.getMusicDirectory,
		"getGenres":                 s.getGenres,
		"getArtist":                 s.getArtist,
		"getArtistInfo":             s.getArtistInfo,
		"getArtistInfo2":            s.getArtistInfo,
		"getAlbum":                  s.getAlbum,
		"getAlbumInfo":              s.getAlbumInfo,
		"getAlbumInfo2":             s.getAlbumInfo,
		"getSong":                   s.getSong,
		"getTopSongs":               s.getTopSongs,
		"getSimilarSongs":           s.getSimilarSongs,
		"getSimilarSongs2":          s.getSimilarSongs,
		"getAlbumList":              s.getAlbumList,
		"getAlbumList2":             s.getAlbumList,
		"getRandomSongs":            s.getRandomSongs,
		"getSongsByGenre":           s.getSongsByGenre,
		"getNowPlaying":             s.getNowPlaying,
		"getStarred":                s.getStarred,
		"getStarred2":               s.getStarred,
		"search":                    s.search,
		"search2":                   s.search,
		"search3":                   s.search,
		"getPlaylists":              s.getPlaylists,
		"getPlaylist":               s.getPlaylist,
		"createPlaylist":            s.createPlaylist,
		"updatePlaylist":            s.updatePlaylist,
		"deletePlaylist":            s.deletePlaylist,
		"stream":                    s.stream,
		"download":                  s.download,
		"getCoverArt":               s.getCoverArt,
		"getLyrics":                 s.getLyrics,
		"getLyricsBySongId":         s.getLyricsBySongID,
		"star":                      s.star,
		"unstar":                    s.unstar,
		"setRating":                 s.setRating,
		"scrobble":                  s.scrobble,
		"getPlayQueue":              s.getPlayQueue,
		"savePlayQueue":             s.savePlayQueue,
		"getBookmarks":              s.getBookmarks,
		"getInternetRadioStations":  s.getInternetRadioStations,
		"getPodcasts":               s.getPodcasts,
		"getNewestPodcasts":         s.getPodcasts,
		"getShares":                 s.getShares,
		"getVideos":                 s.getVideos,
		"getAvatar":                 s.getAvatar,
		"getSongLists":              s.getOnlineSongLists,
	}
	dispatch := func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "method")
		name = strings.TrimSuffix(name, ".view")
		r, finishDiagnostic := s.beginClientDiagnostic(r, name)
		defer finishDiagnostic()
		if event, _ := r.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event); event != nil {
			w.Header().Set("X-Request-ID", event.RequestID)
		}
		h, ok := handlers[name]
		if !ok {
			writeErr(w, r, ErrGeneric, "Unsupported method: "+name)
			return
		}
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		if name != "ping" || param(r, "u") != "" || param(r, "apiKey") != "" {
			identity := "user:" + param(r, "u")
			if key := param(r, "apiKey"); key != "" {
				identity = "key:" + key
			}
			finish, allowed := s.authLimits.Begin(identity, r.RemoteAddr, time.Now())
			if !allowed {
				writeErr(w, r, ErrAuthLimited, "认证尝试过于频繁，请一分钟后重试")
				return
			}
			u, code, msg := s.authenticate(r)
			finish(u != nil)
			if u == nil {
				if name == "ping" && code == ErrMissingParam {
					h(w, r)
					return
				}
				writeErr(w, r, code, msg)
				return
			}
			r = r.WithContext(withUser(r.Context(), u))
		}
		h(w, r)
	}
	r.Get("/{method}", dispatch)
	r.Post("/{method}", dispatch)
	// 播放器可能先用 HEAD 校验链接；只为媒体开放，避免预检触发修改接口。
	r.Head("/{method}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(chi.URLParam(r, "method"), ".view")
		if name != "stream" && name != "download" {
			w.Header().Set("Allow", "GET, POST")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		dispatch(w, r)
	})
	return r
}
