package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/js"
)

func TestHealthLocalDependenciesAndProbe(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pool, err := js.NewSDKPool(1, "", "", nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	server := httptest.NewServer(healthHandler(d, pool))
	defer server.Close()
	t.Setenv("LXSC_LISTEN", server.Listener.Addr().String())
	t.Setenv("LXSC_PORT", "")
	t.Setenv("LXSC_DATA_DIR", t.TempDir())
	if err := checkHealth(""); err != nil {
		t.Fatalf("健康服务探测失败: %v", err)
	}
	w := httptest.NewRecorder()
	healthHandler(d, nil)(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("SDK 未初始化应返回 503")
	}
	d.Close()
	if err := checkHealth(""); err == nil {
		t.Fatal("数据库关闭后探测应失败")
	}
}

func TestHealthListenAddresses(t *testing.T) {
	for address, want := range map[string]string{":8080": "http://127.0.0.1:8080/healthz", "0.0.0.0:9000": "http://127.0.0.1:9000/healthz", "[::]:8080": "http://[::1]:8080/healthz", "127.0.0.1:1234": "http://127.0.0.1:1234/healthz"} {
		got, err := healthURL(address)
		if err != nil || got != want {
			t.Fatalf("监听地址转换错误: %s %v", got, err)
		}
	}
}
