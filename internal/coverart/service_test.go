package coverart

import (
	"bytes"
	"context"
	"errors"
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
	"time"

	"lxsc/internal/diagnostics"
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
	s := &Service{HTTP: httpguard.NewPublicClient(func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "private.example" {
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	})}
	for _, target := range []string{"http://127.0.0.1/a", "http://private.example/a", "http://media.example/redirect", "http://media.example/html", "http://media.example/svg", "http://media.example/large", "file:///tmp/secret"} {
		r := httptest.NewRequest("GET", "/rest/getCoverArt", nil)
		w := httptest.NewRecorder()
		s.Serve(w, r, Resource{UserID: 1, Address: target}, nil)
		if w.Code != 502 || strings.Contains(w.Body.String(), target) {
			t.Fatalf("未安全拒绝 %s: %d %s", target, w.Code, w.Body.String())
		}
	}
	if requests.Load() != 4 {
		t.Fatal("禁止的目标不应发起网络请求", requests.Load())
	}
	r := httptest.NewRequest("GET", "/rest/getCoverArt?apiKey=SECRET", nil)
	w := httptest.NewRecorder()
	s.Serve(w, r, Resource{UserID: 1, Address: "http://media.example/image"}, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(w.Body.Bytes(), picture.Bytes()) {
		t.Fatal(w.Code, w.Header())
	}
	if s.Limits.Stats().Active != 0 {
		t.Fatal("封面额度未释放")
	}
}

func waitCoverCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("封面并发状态未按预期推进")
		}
		time.Sleep(time.Millisecond)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCoverStaleFallbackKeepsRealImage(t *testing.T) {
	for _, cause := range []error{errors.New("temporary failure"), httpguard.ErrUnsafeTarget, httpguard.ErrRedirectLimit} {
		t.Run(cause.Error(), func(t *testing.T) {
			var picture bytes.Buffer
			if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			s := &Service{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, cause
			})}}
			s.init()
			resource := Resource{UserID: 1, Address: "https://image.example/cover", Fields: map[string]string{"title": "synthetic"}}
			key := resource.Address + "\x00"
			c := s.cache
			c.mu.Lock()
			c.putLocked(key, coverImage{data: picture.Bytes(), contentType: "image/png", origin: "original"})
			entry, _ := c.entries.Peek(key)
			entry.expires = time.Now().Add(-time.Minute)
			c.entries.Add(key, entry)
			c.mu.Unlock()
			r := httptest.NewRequest(http.MethodGet, "/cover", nil)
			event := &diagnostics.Event{Format: "binary"}
			w := httptest.NewRecorder()
			capture, finish := diagnostics.CaptureResponse(w, r, event)
			s.Serve(capture, r, resource, event)
			finish()
			if errors.Is(cause, httpguard.ErrUnsafeTarget) || errors.Is(cause, httpguard.ErrRedirectLimit) {
				if w.Code != http.StatusBadGateway || event.CoverOrigin != "" || event.Result != "failed" {
					t.Fatal("unsafe failures must not be hidden by stale or placeholder images", event)
				}
				return
			}
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), picture.Bytes()) || event.CoverOrigin != "stale" || event.Result != "unavailable" || !event.Cached {
				t.Fatal("暂时失败时未保留近期真实封面", event)
			}
			if event.BytesWritten == nil || event.ResponseBytes == nil || *event.BytesWritten != *event.ResponseBytes {
				t.Fatal("stale delivery lost byte accounting")
			}
		})
	}
}
