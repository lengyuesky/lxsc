package fnmusic

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/httpguard"
)

type userKey struct{}
type sessionKey struct{}

type session struct {
	userID   int64
	password [32]byte
	expires  time.Time
	created  time.Time
}

type sessionStore struct {
	mu      sync.Mutex
	entries map[[32]byte]session
}

func (s *sessionStore) start(u *db.User, now time.Time) (string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(random[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[[32]byte]session{}
	}
	for key, entry := range s.entries {
		if !now.Before(entry.expires) {
			delete(s.entries, key)
		}
	}
	for _, limit := range []struct {
		user int64
		max  int
	}{{u.ID, 10}, {0, 4096}} {
		for {
			count := 0
			var oldest [32]byte
			var first time.Time
			for key, entry := range s.entries {
				if limit.user != 0 && entry.userID != limit.user {
					continue
				}
				count++
				if first.IsZero() || entry.created.Before(first) {
					oldest, first = key, entry.created
				}
			}
			if count < limit.max {
				break
			}
			delete(s.entries, oldest)
		}
	}
	s.entries[sha256.Sum256([]byte(token))] = session{
		userID: u.ID, password: sha256.Sum256([]byte(u.PasswordEnc)), created: now, expires: now.Add(7 * 24 * time.Hour),
	}
	return token, nil
}

func (s *sessionStore) get(token string, now time.Time) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	entry, ok := s.entries[key]
	if ok && !now.Before(entry.expires) {
		delete(s.entries, key)
		return session{}, false
	}
	return entry, ok
}

func (s *sessionStore) end(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, sha256.Sum256([]byte(token)))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		DeviceID string `json:"deviceId"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Username)
	if name == "" || len(name) > 128 || len(body.Password) != 64 || len(body.DeviceID) > 128 {
		failure(w, http.StatusBadRequest, "登录参数错误")
		return
	}
	finish, allowed := s.limits.Begin("fnmusic:"+name, r.RemoteAddr, time.Now())
	if !allowed {
		w.Header().Set("Retry-After", "60")
		failure(w, http.StatusTooManyRequests, "登录尝试过于频繁")
		return
	}
	u, err := s.Auth.AuthenticateSHA256(r.Context(), name, body.Password)
	finish(err == nil)
	if err != nil {
		failure(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	token, err := s.sessions.start(u, time.Now())
	if err != nil {
		failure(w, http.StatusInternalServerError, "无法创建音乐会话")
		return
	}
	setCookie(w, r, token, 7*24*3600)
	success(w, map[string]any{"user": s.userView(u), "userToken": token})
}

func requestToken(r *http.Request) (string, bool) {
	var found string
	add := func(value string) bool {
		if value == "" {
			return true
		}
		if len(value) > 256 || (found != "" && found != value) {
			return false
		}
		found = value
		return true
	}
	for _, cookie := range r.Cookies() {
		if cookie.Name == "music-token" {
			value, err := url.QueryUnescape(cookie.Value)
			if err != nil || !add(value) {
				return "", false
			}
		}
	}
	for _, value := range r.Header.Values("Authorization") {
		if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
			value = strings.TrimSpace(value[7:])
		}
		if len(value) != 64 || !add(value) {
			return "", false
		}
	}
	return found, found != ""
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := requestToken(r)
		entry, valid := s.sessions.get(token, time.Now())
		if !ok || !valid {
			failure(w, http.StatusUnauthorized, "INVALID TOKEN")
			return
		}
		u, err := s.DB.GetUserByID(r.Context(), entry.userID)
		if err != nil || entry.password != sha256.Sum256([]byte(u.PasswordEnc)) {
			s.sessions.end(token)
			failure(w, http.StatusUnauthorized, "INVALID TOKEN")
			return
		}
		ctx := context.WithValue(r.Context(), userKey{}, u)
		ctx = context.WithValue(ctx, sessionKey{}, sha256.Sum256([]byte(token)))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func currentUser(r *http.Request) *db.User {
	u, _ := r.Context().Value(userKey{}).(*db.User)
	return u
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) { success(w, s.userView(currentUser(r))) }

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	token, _ := requestToken(r)
	s.sessions.end(token)
	s.searches.revoke(sha256.Sum256([]byte(token)))
	setCookie(w, r, "", -1)
	success(w, map[string]any{})
}

func setCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: "music-token", Value: token, Path: "/music", HttpOnly: true, Secure: httpguard.Secure(r), SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}
