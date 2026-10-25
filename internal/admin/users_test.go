package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/db"
	"lxsc/internal/diagnostics"
	"lxsc/internal/secret"
	"lxsc/internal/webauth"
)

func newUserManagementServer(t *testing.T) (*Server, *db.User, *http.Cookie) {
	t.Helper()
	s := newAdminStabilityServer(t)
	box, err := secret.Load(t.TempDir(), "users-test-key")
	if err != nil {
		t.Fatal(err)
	}
	s.Secret = box
	s.Auth = &webauth.Manager{DB: s.DB, Secret: box}
	enc, err := box.Encrypt("admin-password")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.DB.CreateUser(context.Background(), "admin", enc, true, "320k")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.Auth.Start(w, httptest.NewRequest("GET", "http://users.test", nil), owner)
	return s, owner, w.Result().Cookies()[0]
}

func userManagementRequest(handler http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://users.test"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestCreateUserWithRandomOrManualPassword(t *testing.T) {
	s, _, cookie := newUserManagementServer(t)
	handler := s.Routes()
	seen := map[string]bool{}
	for _, tc := range []struct {
		body, password string
		random         bool
	}{
		{`{"name":"manual","password":"manual-password","quality":"128k"}`, "manual-password", false},
		{`{"name":"manual-explicit","password":"another-password","randomPassword":false}`, "another-password", false},
		{`{"name":"random-one","randomPassword":true,"quality":"flac"}`, "", true},
		{`{"name":"random-two","randomPassword":true,"password":"","isAdmin":true}`, "", true},
	} {
		w := userManagementRequest(handler, "POST", "/users", tc.body, cookie)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("创建失败: %d %s", w.Code, w.Body)
		}
		var result struct {
			db.User
			Password string `json:"password"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		password := tc.password
		if tc.random {
			password = result.Password
			if !regexp.MustCompile(`^[A-Za-z0-9_-]{24}$`).MatchString(password) || seen[password] {
				t.Fatal("随机密码应足够长且每次独立生成")
			}
			seen[password] = true
		} else if result.Password != "" || strings.Contains(w.Body.String(), `"password"`) {
			t.Fatal("手动密码不应在响应中回显")
		}
		stored, err := s.DB.GetUserByID(context.Background(), result.ID)
		if err != nil || stored.PasswordEnc == password || strings.Contains(w.Body.String(), stored.PasswordEnc) {
			t.Fatal("明文及密文不得泄露至普通用户响应", err)
		}
		plain, err := s.Secret.Decrypt(stored.PasswordEnc)
		if err != nil || plain != password {
			t.Fatal("返回的密码必须与保存的密码一致", err)
		}
		if authenticated, err := s.Auth.Authenticate(context.Background(), result.Name, password); err != nil || authenticated.ID != result.ID {
			t.Fatal("新建账号必须能使用该密码登录", err)
		}
	}
	w := userManagementRequest(handler, "GET", "/users", "", cookie)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"password"`) || strings.Contains(w.Body.String(), `password_enc`) {
		t.Fatal("用户列表不能再次返回密码", w.Code, w.Body)
	}
	for password := range seen {
		if strings.Contains(w.Body.String(), password) {
			t.Fatal("用户列表泄露随机密码")
		}
	}
}

func TestRandomUserPasswordValidationAndPermissions(t *testing.T) {
	s, _, cookie := newUserManagementServer(t)
	handler := s.Routes()
	for _, body := range []string{
		`null`, `{}`, `{"name":"missing-password"}`,
		`{"name":"manual-empty","password":"","randomPassword":false}`,
		`{"name":" ","randomPassword":true}`,
		`{"name":"mixed","password":"must-not-ignore","randomPassword":true}`,
		`{"name":"wrong-type","randomPassword":"true"}`,
		`{"name":"unknown","randomPassword":true,"userId":1}`,
		`{"name":"trailing","randomPassword":true} {}`,
	} {
		w := userManagementRequest(handler, "POST", "/users", body, cookie)
		if w.Code != 400 || strings.Contains(w.Body.String(), `"password"`) {
			t.Errorf("无效创建参数应失败且不得返回密码: %s %d %s", body, w.Code, w.Body)
		}
	}
	if count, err := s.DB.CountUsers(context.Background()); err != nil || count != 1 {
		t.Fatal("校验失败不能创建账号", count, err)
	}
	if w := userManagementRequest(handler, "POST", "/users", `{"name":"guest","randomPassword":true}`, nil); w.Code != 401 {
		t.Fatal("匿名用户不能创建账号", w.Code)
	}
	enc, _ := s.Secret.Encrypt("normal-password")
	normal, err := s.DB.CreateUser(context.Background(), "normal", enc, false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.Auth.Start(w, httptest.NewRequest("GET", "/", nil), normal)
	normalCookie := w.Result().Cookies()[0]
	if w := userManagementRequest(handler, "POST", "/users", `{"name":"guest","randomPassword":true}`, normalCookie); w.Code != 403 {
		t.Fatal("普通用户只能改本人密码，不能创建用户", w.Code)
	}
}

func TestSelfPasswordChangeRevokesAdminDebugTokens(t *testing.T) {
	s, owner, cookie := newUserManagementServer(t)
	debug := diagnostics.New(s.DB, s.Auth, s.Catalog, s.Sources, s.Settings, "test", time.Now())
	defer debug.Tokens.Close()
	_, _, err := debug.Tokens.Create(owner.ID, 900, false)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Mount("/api/auth", (&webauth.Server{Auth: s.Auth, Settings: s.Settings, PasswordChanged: debug.Tokens.RevokeOwner}).Routes())
	w := userManagementRequest(r, "PUT", "/api/auth/password", `{"currentPassword":"admin-password","newPassword":"changed-admin-password"}`, cookie)
	if w.Code != 200 {
		t.Fatal("管理员也应能修改本人密码", w.Code, w.Body)
	}
	views, _ := debug.Tokens.List()
	if len(views) != 1 || !views[0].Revoked {
		t.Fatal("管理员本人改密也必须撤销已有调试凭据", views)
	}
}
