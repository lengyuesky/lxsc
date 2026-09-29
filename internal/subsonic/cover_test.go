package subsonic

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"lxsc/internal/db"
	"lxsc/internal/httpguard"
)

func TestCoverRejectsUnsafeTargetsAndActiveContent(t *testing.T) {
	var requests atomic.Int32
	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Error("封面不应携带用户凭据")
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "http://127.0.0.1/private", 302)
		case "/html":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "<html><script>alert(1)</script></html>")
		case "/svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`)
		case "/large":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxCoverBytes+1))
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(picture.Bytes())
		}
	}))
	defer upstream.Close()
	s := &Server{coverHTTP: httpguard.NewPublicClient(func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "private.example" {
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	})}
	for _, target := range []string{"http://127.0.0.1/a", "http://private.example/a", "http://media.example/redirect", "http://media.example/html", "http://media.example/svg", "http://media.example/large", "file:///tmp/secret"} {
		r := httptest.NewRequest("GET", "/rest/getCoverArt", nil)
		r = r.WithContext(withUser(r.Context(), &db.User{ID: 1}))
		w := httptest.NewRecorder()
		s.proxyCover(w, r, target)
		if w.Code != 502 || strings.Contains(w.Body.String(), target) {
			t.Fatalf("未安全拒绝 %s: %d %s", target, w.Code, w.Body.String())
		}
	}
	if requests.Load() != 4 {
		t.Fatal("禁止的目标不应发起网络请求", requests.Load())
	}
	r := httptest.NewRequest("GET", "/rest/getCoverArt?apiKey=SECRET", nil)
	r = r.WithContext(withUser(r.Context(), &db.User{ID: 1}))
	w := httptest.NewRecorder()
	s.proxyCover(w, r, "http://media.example/image")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(w.Body.Bytes(), picture.Bytes()) {
		t.Fatal(w.Code, w.Header())
	}
	if s.coverLimits.Stats().Active != 0 {
		t.Fatal("封面额度未释放")
	}
}
