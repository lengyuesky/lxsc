package subsonic

import (
	"net/http"
	"time"
)

func (s *Server) ping(w http.ResponseWriter, r *http.Request) { writeOK(w, r, "", nil) }

func (s *Server) getLicense(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "license", M{"valid": true, "email": "lxsc@localhost", "licenseExpires": time.Now().AddDate(10, 0, 0).UTC().Format(time.RFC3339)})
}

func (s *Server) getOpenSubsonicExtensions(w http.ResponseWriter, r *http.Request) {
	ext := []M{
		{"name": "formPost", "versions": []int{1}},
		{"name": "songLyrics", "versions": []int{1}},
	}
	if detectFormat(r) == "xml" {
		writeOK(w, r, "openSubsonicExtensions", ext)
		return
	}
	writeOK(w, r, "openSubsonicExtensions", ext)
}

func (s *Server) userObj(name string, admin bool) M {
	return M{
		"username": name, "email": name + "@lxsc", "scrobblingEnabled": true, "adminRole": admin,
		"settingsRole": admin, "downloadRole": true, "uploadRole": false, "playlistRole": true, "coverArtRole": false,
		"commentRole": false, "podcastRole": false, "streamRole": true, "jukeboxRole": false, "shareRole": false,
		"videoConversionRole": false, "folder": []int{1},
	}
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	name := param(r, "username")
	if name == "" || name == u.Name {
		writeOK(w, r, "user", s.userObj(u.Name, u.IsAdmin))
		return
	}
	if !u.IsAdmin {
		writeErr(w, r, ErrNotAuthorized, "not authorized")
		return
	}
	t, err := s.DB.GetUserByName(r.Context(), name)
	if err != nil {
		writeErr(w, r, ErrNotFound, "user not found")
		return
	}
	writeOK(w, r, "user", s.userObj(t.Name, t.IsAdmin))
}

func (s *Server) getUsers(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.IsAdmin {
		writeErr(w, r, ErrNotAuthorized, "not authorized")
		return
	}
	list, _ := s.DB.ListUsers(r.Context())
	out := make([]M, 0, len(list))
	for _, x := range list {
		out = append(out, s.userObj(x.Name, x.IsAdmin))
	}
	writeOK(w, r, "users", M{"user": out})
}

func (s *Server) getScanStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "scanStatus", M{"scanning": false, "count": 0})
}

func (s *Server) getNowPlaying(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "nowPlaying", M{"entry": []M{}})
}

func (s *Server) getPlayQueue(w http.ResponseWriter, r *http.Request) { writeOK(w, r, "", nil) }
func (s *Server) getBookmarks(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "bookmarks", M{"bookmark": []M{}})
}
func (s *Server) getInternetRadioStations(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "internetRadioStations", M{"internetRadioStation": []M{}})
}
func (s *Server) getPodcasts(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "podcasts", M{"channel": []M{}})
}
func (s *Server) getShares(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "shares", M{"share": []M{}})
}
func (s *Server) getVideos(w http.ResponseWriter, r *http.Request) {
	writeOK(w, r, "videos", M{"video": []M{}})
}
func (s *Server) getAvatar(w http.ResponseWriter, r *http.Request) {
	writeErr(w, r, ErrNotFound, "no avatar")
}
