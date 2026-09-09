package diagnostics

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestTargetAddressPolicy(t *testing.T) {
	for _, address := range []string{"http://127.0.0.1", "http://10.0.0.1", "http://172.16.0.1", "http://192.168.0.1", "http://169.254.169.254", "http://100.100.100.200", "http://0.0.0.0", "http://192.0.2.1", "http://198.18.1.1", "http://240.0.0.1", "http://224.0.0.1", "http://[::1]", "http://[::ffff:8.8.8.8]", "http://[fe80::1]", "http://[fc00::1]", "http://[2001:db8::1]", "http://[2002:0808:0808::1]", "http://[64:ff9b::808:808]", "http://[3fff::1]", "ftp://8.8.8.8", "file:///etc/passwd", "http://user:pass@8.8.8.8", "http://8.8.8.8:8080", "http://8.8.8.8:", "http://8.8.8.8/#fragment", "http://[fe80::1%25eth0]"} {
		u, err := url.Parse(address)
		if err == nil && validTarget(u) {
			t.Error("应拒绝", address)
		}
	}
	for _, address := range []string{"https://media.example/path?signature=secret", "http://8.8.8.8:80/a", "https://[2606:4700:4700::1111]:443/a"} {
		u, _ := url.Parse(address)
		if !validTarget(u) {
			t.Error("应允许", address)
		}
	}
}
func TestDNSPinningMixedAnswersAndNoProxy(t *testing.T) {
	var lookups, dials atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(206) }))
	defer upstream.Close()
	address := strings.TrimPrefix(upstream.URL, "http://")
	client := newProbeClient(func(context.Context, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, target string) (net.Conn, error) {
		dials.Add(1)
		if target != "8.8.8.8:80" {
			t.Errorf("未绑定校验后的IP: %s", target)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	})
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	resp, err := client.Get("http://media.example/audio")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if lookups.Load() != 1 || dials.Load() != 1 {
		t.Fatal(lookups.Load(), dials.Load())
	}
	client = newProbeClient(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}, func(context.Context, string, string) (net.Conn, error) {
		t.Error("混合私网解析不应连接")
		return nil, errors.New("禁止")
	})
	if _, err := client.Get("http://media.example"); ErrorCode(err) != "blocked_target" {
		t.Fatal(ErrorCode(err))
	}
	client = newProbeClient(func(context.Context, string) ([]netip.Addr, error) {
		return nil, &net.DNSError{Err: "含签名URL的恶意自由文本", Name: "SECRET", IsNotFound: true}
	}, func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("DNS失败不应连接")
		return nil, nil
	})
	if _, err := client.Get("http://media.example?secret=SECRET"); ErrorCode(err) != "dns" {
		t.Fatal(ErrorCode(err))
	}
}
func TestRedirectValidationRebindingAndLimit(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/SECRET", "http://private.example/SECRET", "http://rebind.example/SECRET", "http://user:pass@8.8.8.8/SECRET", "http://8.8.8.8:8080/SECRET", "http://media.example/again"} {
		t.Run(target, func(t *testing.T) {
			var calls, lookups atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Redirect(w, r, target, 302) }))
			defer upstream.Close()
			client := newProbeClient(func(_ context.Context, host string) ([]netip.Addr, error) {
				n := lookups.Add(1)
				if host == "private.example" || (host == "rebind.example" && n > 1) {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}, func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
			})
			resp, err := client.Get("http://media.example/start")
			if resp != nil {
				resp.Body.Close()
			}
			want := "blocked_target"
			count := int32(1)
			if target == "http://media.example/again" {
				want = "redirect_limit"
				count = 4
			}
			if ErrorCode(err) != want || calls.Load() != count {
				t.Fatal(ErrorCode(err), calls.Load())
			}
		})
	}
}
func TestProbeTLSVerification(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("自签名TLS不应通过") }))
	defer upstream.Close()
	client := newProbeClient(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "https://"))
	})
	resp, err := client.Get("https://media.example/secret?key=SECRET")
	if resp != nil {
		resp.Body.Close()
	}
	if ErrorCode(err) != "tls" {
		t.Fatal(ErrorCode(err))
	}
}
func TestEventsWhitelistAndSafeErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"},
		{&net.DNSError{Err: "SECRET", Name: "http://name:pass@private/secret", IsNotFound: true}, "dns"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connect"},
		{x509.UnknownAuthorityError{}, "tls"}, {errors.New("https://user:pass@private/secret?token=SECRET\n头部与日志"), "upstream_error"},
	} {
		wrapped := tc.err
		if wrapped != nil {
			wrapped = &url.Error{Op: "Get", URL: "https://SECRET:password@example/private?secret=YES", Err: wrapped}
		}
		if code := ErrorCode(wrapped); code != tc.want {
			t.Fatal(code, tc.want)
		}
	}
	events := &Events{}
	for i := 0; i < 250; i++ {
		events.Add(Event{Stage: "cache_check", TrackID: "tr-wy-1?token=SECRET", Platform: "SECRET", Quality: "SECRET", Mode: "SECRET", Error: "SECRET", Status: -1, ElapsedMS: -1})
	}
	events.Add(Event{Stage: "SECRET"})
	list := events.List()
	if len(list) != 200 {
		t.Fatal(len(list))
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal("诊断字段泄露")
	}
	if list[0].Status != 0 || list[0].Error != "upstream_error" {
		t.Fatal(list[0])
	}
	// 不转发自定义媒体类型、任意 ETag、响应路径或畸形响应头。
	headers := mediaHeaders(http.Header{"Content-Type": {"text/html;secret=YES"}, "Content-Range": {"bytes SECRET"}, "Content-Length": {"-1"}, "Accept-Ranges": {"SECRET"}, "Etag": {"SECRET"}})
	if len(headers) != 0 {
		t.Fatal(headers)
	}
}

func TestRedirectDoesNotForwardSignedRefererAndHeadersAreBounded(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Referer") != "" || r.Header.Get("Authorization") != "" {
			t.Error("泄露上跳凭据", r.Header)
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "http://other.example/final", 302)
			return
		}
		if r.URL.Path == "/large" {
			w.Header().Set("X-Large", strings.Repeat("a", 32<<10))
		}
		w.WriteHeader(206)
	}))
	defer upstream.Close()
	client := newProbeClient(func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(upstream.URL, "http://"))
	})
	resp, err := client.Get("http://media.example/start?signature=SECRET")
	if err != nil {
		t.Fatal(ErrorCode(err))
	}
	resp.Body.Close()
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
	resp, err = client.Get("http://media.example/large")
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("响应头大小未限制")
	}
}
