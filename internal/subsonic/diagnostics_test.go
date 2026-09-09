package subsonic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lxsc/internal/diagnostics"
)

func TestPlaybackDiagnosticsStatusZeroAndForceRedirect(t *testing.T) {
	for _, mode := range []string{"redirect", "force_redirect", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			s, user, info := newMediaStabilityServer(t, recoveryScript)
			s.Diagnostics = &diagnostics.Events{}
			raw, _ := json.Marshal(mode)
			if _, err := s.Settings.Update(context.Background(), map[string]json.RawMessage{"streamMode": raw}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Catalog.ResolvePlaybackURL(context.Background(), info, "320k"); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, &net.DNSError{Err: "SECRET https://user:pass@host/path?key=PRIVATE", Name: "PRIVATE", IsNotFound: true}
			})}
			w := httptest.NewRecorder()
			s.stream(w, mediaStabilityRequest(user, info, "&proxy=1"))
			found := false
			for _, e := range s.Diagnostics.List() {
				if e.Stage == "cache_check" || e.Stage == "proxy_response" {
					if e.Status != 0 || e.Error != "dns" {
						t.Fatal(e)
					}
					found = true
				}
				if mode == "force_redirect" && strings.HasPrefix(e.Stage, "proxy") {
					t.Fatal("强制302出现代理事件", e)
				}
			}
			if !found || calls != 1 {
				t.Fatal(found, calls)
			}
			if mode == "force_redirect" && w.Code != 302 {
				t.Fatal("网络不确定不能阻止客户端直连", w.Code)
			}
			raw, _ = json.Marshal(s.Diagnostics.List())
			for _, secret := range []string{"SECRET", "PRIVATE", "https://", user.Name, info.Name()} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("诊断泄露", secret, string(raw))
				}
			}
		})
	}
}
func TestPlaybackDiagnosticsProxyCopyAndRefresh(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	s.Diagnostics = &diagnostics.Events{}
	calls := 0
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		status := 200
		if calls == 1 {
			status = 403
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("合成音频")), Request: r}, nil
	})}
	w := httptest.NewRecorder()
	s.stream(w, mediaStabilityRequest(user, info, "&proxy=1"))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	stages := map[string]bool{}
	for _, e := range s.Diagnostics.List() {
		stages[e.Stage] = true
	}
	for _, stage := range []string{"metadata", "resolve", "refresh", "proxy_response", "proxy_copy"} {
		if !stages[stage] {
			t.Fatal("缺少阶段", stage)
		}
	}
	s.Diagnostics.Add(diagnostics.Event{Stage: "proxy_response", Error: diagnostics.ErrorCode(errors.New("任意敏感错误文本"))})
}
