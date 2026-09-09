package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDebugPathsNeverReceiveGlobalCORS(t *testing.T) {
	handler := cors(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for _, path := range []string{"/api/debug", "/api/debug/", "/api/debug/status", "/api/debug/probe/", "/api/admin/debug-tokens", "/api/admin/debug-tokens/", "/api/admin/debug-tokens/id"} {
		for _, method := range []string{"GET", "POST", "OPTIONS"} {
			r := httptest.NewRequest(method, path, nil)
			r.Header.Set("Origin", "https://evil.test")
			r.Header.Set("Access-Control-Request-Headers", "X-LXSC-Debug-Management")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Headers") != "" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(path, w.Header())
			}
			if method == "OPTIONS" && w.Code != 403 {
				t.Fatal(path, w.Code)
			}
		}
	}
	for _, path := range []string{"/rest/ping.view", "/api/app/playlists", "/api/admin/status", "/api/debug-other", "/api/admin/debug-tokens-other"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("OPTIONS", path, nil))
		if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal(path, w.Code, w.Header())
		}
	}
}

func TestDebugPreflightSlowBodyClosesThroughRequestLogger(t *testing.T) {
	handler := requestLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))(cors(http.NotFoundHandler()))
	server := httptest.NewServer(handler)
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, err = fmt.Fprint(conn, "OPTIONS /api/debug/probe HTTP/1.1\r\nHost: debug.test\r\nOrigin: https://other.test\r\nContent-Length: 2000\r\n\r\n{")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal("预检拒绝仍排空慢正文", err)
	}
	if !strings.HasPrefix(string(raw), "HTTP/1.1 403 ") || strings.Contains(string(raw), "Access-Control-Allow-") {
		t.Fatalf("无效安全响应: %q", raw)
	}
}
