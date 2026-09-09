package admin

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/portal"
	"lxsc/internal/subsonic"
	"lxsc/internal/webauth"
)

func TestDebugCredentialAuthenticationIsolationAndOwnerMutation(t *testing.T) {
	s := newAdminStabilityServer(t)
	s.Auth = &webauth.Manager{DB: s.DB}
	owner, err := s.DB.CreateUser(context.Background(), "owner", "", true, "320k")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := s.DB.CreateUser(context.Background(), "manager", "", true, "320k")
	if err != nil {
		t.Fatal(err)
	}
	normal, err := s.DB.CreateUser(context.Background(), "normal", "", false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	s.Debug = diagnostics.New(s.DB, s.Auth, s.Catalog, s.Sources, s.Settings, "test", time.Now())
	t.Cleanup(s.Debug.Tokens.Close)
	r := chi.NewRouter()
	r.Mount("/api/admin", s.Routes())
	r.Mount("/api/debug", s.Debug.Routes())
	app := &portal.Server{DB: s.DB, Auth: s.Auth, Settings: s.Settings}
	r.Mount("/api/app", app.Routes())
	media := &subsonic.Server{DB: s.DB, Settings: s.Settings}
	r.Mount("/rest", media.Routes())
	cookie := func(u *db.User) *http.Cookie {
		w := httptest.NewRecorder()
		s.Auth.Start(w, httptest.NewRequest("GET", "http://debug.test", nil), u)
		return w.Result().Cookies()[0]
	}
	call := func(method, path, body, key string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://debug.test"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://debug.test")
		req.Header.Set("X-LXSC-Debug-Management", "1")
		if c != nil {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	_, key, err := s.Debug.Tokens.Create(owner.ID, 900, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/admin/status", "/api/admin/debug-tokens", "/api/app/playlists", "/api/app/me"} {
		w := call("GET", path, "", key, nil)
		if w.Code != 401 {
			t.Fatal(path, w.Code, w.Body)
		}
	}
	w := call("GET", "/rest/getUser.view?f=json", "", key, nil)
	if !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatal("调试凭据不应支持Subsonic", w.Code, w.Body)
	}
	if err := s.DB.CreateAPIKey(context.Background(), owner.ID, "lxsc_synthetic_key", "test"); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/api/debug/status", "", "lxsc_synthetic_key", nil); w.Code != 401 {
		t.Fatal("普通API key不能调试", w.Code)
	}
	if w := call("POST", "/api/admin/debug-tokens", "{}", key, cookie(normal)); w.Code != 403 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("普通用户不能管理", w.Code, w.Header())
	}
	if w := call("PUT", fmt.Sprintf("/api/admin/users/%d", owner.ID), `{"name":"owner","isAdmin":false,"quality":"320k"}`, "", cookie(manager)); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	views, _ := s.Debug.Tokens.List()
	if len(views) != 1 || !views[0].Revoked {
		t.Fatal("降权未立即撤销", views)
	}
	if err := s.DB.UpdateUser(context.Background(), owner.ID, "owner", "", true, "320k"); err != nil {
		t.Fatal(err)
	}
	_, key, err = s.Debug.Tokens.Create(owner.ID, 900, false)
	if err != nil {
		t.Fatal(err)
	}
	if w := call("DELETE", fmt.Sprintf("/api/admin/users/%d", owner.ID), "", "", cookie(manager)); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call("GET", "/api/debug/status", "", key, nil); w.Code != 401 {
		t.Fatal("删除创建者必须失效", w.Code)
	}
}

func TestDebugManagementOuterAuthClosesSlowBody(t *testing.T) {
	s := newAdminStabilityServer(t)
	s.Auth = &webauth.Manager{DB: s.DB}
	router := chi.NewRouter()
	router.Mount("/api/admin", s.Routes())
	server := httptest.NewServer(router)
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, err = fmt.Fprint(conn, "POST /api/admin/debug-tokens HTTP/1.1\r\nHost: debug.test\r\nContent-Length: 2000\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal("外层管理员拒绝仍等待正文", err)
	}
	if !strings.HasPrefix(string(raw), "HTTP/1.1 401 ") {
		t.Fatalf("无效响应: %q", raw)
	}
}
