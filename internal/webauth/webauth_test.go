package webauth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/admin"
	"lxsc/internal/backup"
	"lxsc/internal/db"
	"lxsc/internal/music"
	"lxsc/internal/portal"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
	"lxsc/internal/webauth"
)

func TestUnifiedSessionAndPermissions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	box, err := secret.Load(dir, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name  string
		admin bool
	}{{"admin", true}, {"alice", false}} {
		enc, _ := box.Encrypt("pw-" + item.name)
		if _, err := database.CreateUser(ctx, item.name, enc, item.admin, "320k"); err != nil {
			t.Fatal(err)
		}
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authManager := &webauth.Manager{DB: database, Secret: box}
	authServer := &webauth.Server{Auth: authManager, Settings: store}
	catalog := music.NewCatalog(database, nil, nil, store, logger)
	portalServer := &portal.Server{DB: database, Catalog: catalog, Settings: store, Secret: box, Log: logger, Auth: authManager}
	backupServer := backup.New(database, box, dir, "test", nil, logger)
	adminServer := &admin.Server{DB: database, Settings: store, Secret: box, Auth: authManager, Backup: backupServer}

	router := chi.NewRouter()
	router.Mount("/api/auth", authServer.Routes())
	router.Mount("/api/app", portalServer.Routes())
	router.Mount("/api/admin", adminServer.Routes())
	ts := httptest.NewServer(router)
	defer ts.Close()

	alice := clientWithJar(t)
	if status := jsonRequest(t, alice, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "alice", "password": "pw-alice"}); status != http.StatusOK {
		t.Fatalf("普通用户登录失败: %d", status)
	}
	if status := jsonRequest(t, alice, http.MethodGet, ts.URL+"/api/app/playlists", nil); status != http.StatusOK {
		t.Fatalf("同一会话应可访问歌单接口: %d", status)
	}
	if status := jsonRequest(t, alice, http.MethodGet, ts.URL+"/api/admin/settings", nil); status != http.StatusForbidden {
		t.Fatalf("普通用户访问管理接口应返回 403，实际为 %d", status)
	}
	if status := jsonRequest(t, alice, http.MethodGet, ts.URL+"/api/admin/backups/status", nil); status != http.StatusForbidden {
		t.Fatalf("普通用户访问备份接口应返回 403，实际为 %d", status)
	}

	adminClient := clientWithJar(t)
	if status := jsonRequest(t, adminClient, http.MethodPost, ts.URL+"/api/auth/login", map[string]any{"username": "admin", "password": "pw-admin"}); status != http.StatusOK {
		t.Fatalf("管理员登录失败: %d", status)
	}
	if status := jsonRequest(t, adminClient, http.MethodGet, ts.URL+"/api/app/playlists", nil); status != http.StatusOK {
		t.Fatalf("管理员会话应可访问歌单接口: %d", status)
	}
	if status := jsonRequest(t, adminClient, http.MethodGet, ts.URL+"/api/admin/settings", nil); status != http.StatusOK {
		t.Fatalf("管理员会话应可访问管理接口: %d", status)
	}
	if status := jsonRequest(t, adminClient, http.MethodGet, ts.URL+"/api/admin/backups/status", nil); status != http.StatusOK {
		t.Fatalf("管理员会话应可访问备份接口: %d", status)
	}
	if status := jsonRequest(t, adminClient, http.MethodPost, ts.URL+"/api/auth/logout", nil); status != http.StatusOK {
		t.Fatalf("注销失败: %d", status)
	}
	if status := jsonRequest(t, adminClient, http.MethodGet, ts.URL+"/api/app/playlists", nil); status != http.StatusUnauthorized {
		t.Fatalf("注销后歌单接口应返回 401，实际为 %d", status)
	}
	if status := jsonRequest(t, adminClient, http.MethodGet, ts.URL+"/api/admin/settings", nil); status != http.StatusUnauthorized {
		t.Fatalf("注销后管理接口应返回 401，实际为 %d", status)
	}
}

func TestUnifiedLoginFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	box, _ := secret.Load(dir, "test-key")
	enc, _ := box.Encrypt("right")
	if _, err := database.CreateUser(ctx, "alice", enc, false, "320k"); err != nil {
		t.Fatal(err)
	}
	store, _ := settings.New(ctx, database)
	ts := httptest.NewServer((&webauth.Server{Auth: &webauth.Manager{DB: database, Secret: box}, Settings: store}).Routes())
	defer ts.Close()
	client := clientWithJar(t)
	if status := jsonRequest(t, client, http.MethodPost, ts.URL+"/login", map[string]any{"username": "alice", "password": "wrong"}); status != http.StatusUnauthorized {
		t.Fatalf("错误密码应返回 401，实际为 %d", status)
	}
	if status := jsonRequest(t, client, http.MethodGet, ts.URL+"/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("未登录访问当前用户应返回 401，实际为 %d", status)
	}
}

func clientWithJar(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func jsonRequest(t *testing.T, client *http.Client, method, url string, body any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
