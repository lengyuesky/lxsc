package subsonic

import (
	"context"
	"crypto/md5"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/secret"
)

func TestSubsonicAuthenticationLimitAndCompatibility(t *testing.T) {
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
	enc, _ := box.Encrypt("password")
	user, err := database.CreateUser(context.Background(), "alice", enc, false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CreateAPIKey(context.Background(), user.ID, "test-key", "测试"); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: database, Secret: box}
	handler := s.Routes()
	request := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/ping.view?f=json&"+query, nil)
		r.RemoteAddr = "8.8.8.8:123"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	token := fmt.Sprintf("%x", md5.Sum([]byte("passwordsalt")))
	for range 100 {
		for _, query := range []string{"u=alice&p=password", "u=alice&p=enc:70617373776f7264", "u=alice&t=" + strings.ToUpper(token) + "&s=salt", "apiKey=test-key"} {
			w := request(query)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
				t.Fatal("正常认证应兼容且不消耗失败预算", w.Code, w.Body.String())
			}
		}
	}
	for range 20 {
		w := request("u=alice&p=wrong")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"code":40`) {
			t.Fatal("失败认证应保持Subsonic协议", w.Code, w.Body.String())
		}
	}
	w := request("u=alice&p=wrong")
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "subsonic-response") {
		t.Fatal("认证超限应返回429及协议错误", w.Code, w.Body.String())
	}
}
