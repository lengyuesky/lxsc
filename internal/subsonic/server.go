package subsonic

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
)

// Server Subsonic API 服务
type Server struct {
	DB       *db.DB
	Catalog  *music.Catalog
	Settings *settings.Store
	Secret   *secret.Box
	Log      *slog.Logger
	HTTP     *http.Client // 代理拉流用
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
		"setRating":                 s.noop,
		"scrobble":                  s.scrobble,
		"getPlayQueue":              s.getPlayQueue,
		"savePlayQueue":             s.noop,
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
		h, ok := handlers[name]
		if !ok {
			writeErr(w, r, ErrGeneric, "Unsupported method: "+name)
			return
		}
		if r.Method == http.MethodPost {
			_ = r.ParseForm()
		}
		if name != "ping" || param(r, "u") != "" || param(r, "apiKey") != "" {
			u, code, msg := s.authenticate(r)
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
	return r
}

func (s *Server) noop(w http.ResponseWriter, r *http.Request) { writeOK(w, r, "", nil) }
