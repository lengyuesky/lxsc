package httpguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyTrustBoundary(t *testing.T) {
	prefixes, err := TrustedPrefixes([]string{"10.0.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		peer, chain, proto, want string
		secure                   bool
	}{
		{"8.8.8.8:123", "1.1.1.1", "https", "8.8.8.8:123", false},
		{"10.0.0.1:123", "1.1.1.1, 10.0.0.2", "https", "1.1.1.1", true},
		{"10.0.0.1:123", "1.1.1.1, 8.8.8.8", "http", "8.8.8.8", false},
		{"10.0.0.1:123", "invalid", "https,http", "10.0.0.1:123", false},
		{"10.0.0.1:123", "", "https", "10.0.0.1:123", true},
	} {
		r := httptest.NewRequest("GET", "http://test.example", nil)
		r.RemoteAddr = tc.peer
		r.Header.Set("X-Forwarded-For", tc.chain)
		r.Header.Set("X-Forwarded-Proto", tc.proto)
		r.Header.Set("X-Real-IP", "FAKE")
		ProxyHeaders(prefixes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.RemoteAddr != tc.want || Secure(r) != tc.secure {
				t.Errorf("%+v: %s %v", tc, r.RemoteAddr, Secure(r))
			}
			if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("X-Forwarded-Proto") != "" {
				t.Error("原始代理头未清除")
			}
			if r.Host != "test.example" {
				t.Error("不应改写Host")
			}
		})).ServeHTTP(httptest.NewRecorder(), r)
	}
	if _, err := TrustedPrefixes([]string{"10.0.0.0/33"}); err == nil {
		t.Fatal("无效网段不应通过")
	}
}

func TestSecureIgnoresRawForwardedHeader(t *testing.T) {
	r := httptest.NewRequest("GET", "http://test.example", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if Secure(r) {
		t.Fatal("裸转发头不是可信的HTTPS证据")
	}
	if !Secure(httptest.NewRequest("GET", "https://test.example", nil)) {
		t.Fatal("真实TLS必须安全")
	}
}
