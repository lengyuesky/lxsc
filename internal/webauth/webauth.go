// Package webauth 提供统一 Web 控制台的登录与会话管理。
package webauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/db"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
)

const cookieName = "lxsc_session"

// Manager 管理所有 Web 页面共享的登录会话。
type Manager struct {
	DB     *db.DB
	Secret *secret.Box

	sessions sync.Map // token -> session
}

type session struct {
	userID  int64
	expires time.Time
}

// Server 提供统一认证接口。
type Server struct {
	Auth     *Manager
	Settings *settings.Store
}

// UserView 是暴露给 Web 控制台的用户信息。
type UserView struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	IsAdmin bool   `json:"isAdmin"`
	Quality string `json:"quality"`
}

// View 转换数据库用户对象。
func View(u *db.User) UserView {
	return UserView{ID: u.ID, Name: u.Name, IsAdmin: u.IsAdmin, Quality: u.Quality}
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]any{"error": message})
}

// Authenticate 校验用户名和密码。
func (m *Manager) Authenticate(ctx context.Context, username, password string) (*db.User, error) {
	u, err := m.DB.GetUserByName(ctx, strings.TrimSpace(username))
	if err != nil {
		return nil, err
	}
	plain, err := m.Secret.Decrypt(u.PasswordEnc)
	if err != nil || subtle.ConstantTimeCompare([]byte(plain), []byte(password)) != 1 {
		return nil, db.ErrNotFound
	}
	return u, nil
}

// Start 创建会话并设置 Cookie。
func (m *Manager) Start(w http.ResponseWriter, r *http.Request, u *db.User) {
	token := secret.RandomToken(24)
	m.sessions.Store(token, session{userID: u.ID, expires: time.Now().Add(7 * 24 * time.Hour)})
	setCookie(w, r, token, 7*24*3600)
}

// End 注销当前会话。
func (m *Manager) End(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		m.sessions.Delete(cookie.Value)
	}
	setCookie(w, r, "", -1)
}

// User 返回请求对应的当前用户，会话无效时返回 nil。
func (m *Manager) User(r *http.Request) *db.User {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	value, ok := m.sessions.Load(cookie.Value)
	if !ok {
		return nil
	}
	ss := value.(session)
	if time.Now().After(ss.expires) {
		m.sessions.Delete(cookie.Value)
		return nil
	}
	u, err := m.DB.GetUserByID(r.Context(), ss.userID)
	if err != nil {
		m.sessions.Delete(cookie.Value)
		return nil
	}
	return u
}

func setCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

// Routes 返回 /api/auth 下的统一认证接口。
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/login", s.login)
	r.Post("/logout", s.logout)
	r.Get("/me", s.me)
	return r
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	u, err := s.Auth.Authenticate(r.Context(), body.Username, body.Password)
	if err != nil {
		fail(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	s.Auth.Start(w, r, u)
	s.writeSession(w, u)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.Auth.End(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := s.Auth.User(r)
	if u == nil {
		fail(w, http.StatusUnauthorized, "未登录或会话已过期")
		return
	}
	s.writeSession(w, u)
}

func (s *Server) writeSession(w http.ResponseWriter, u *db.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user":          View(u),
		"defaultPublic": s.Settings.Get().PublicPlaylists,
	})
}
