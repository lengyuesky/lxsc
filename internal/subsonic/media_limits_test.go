package subsonic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lxsc/internal/admission"
)

func TestMediaTransferQuotaIsSeparateAndReleased(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	var permits []*admission.Permit
	for range 4 {
		p, err := s.Catalog.MediaLimits.Reserve(user.ID)
		if err != nil {
			t.Fatal(err)
		}
		permits = append(permits, p)
	}
	s.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("没有传输额度时不应请求上游")
		return nil, context.Canceled
	})}
	w := httptest.NewRecorder()
	s.stream(w, mediaStabilityRequest(user, info, "&proxy=1"))
	if w.Code != 503 || s.Catalog.RequestLimits.Stats().Active != 0 {
		t.Fatal("传输过载必须及时释放准备额度", w.Code)
	}
	for _, p := range permits {
		p.Release()
	}
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &contextMediaBody{ctx: r.Context(), entered: entered}, Request: r}, nil
	})}
	done := make(chan struct{})
	go func() {
		r := mediaStabilityRequest(user, info, "&proxy=1")
		r = r.WithContext(withUser(ctx, user))
		s.stream(httptest.NewRecorder(), r)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("传输未开始")
	}
	if s.Catalog.RequestLimits.Stats().Active != 0 || s.Catalog.MediaLimits.Stats().Active != 1 {
		t.Fatal("开始传输后应只占用独立媒体额度")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("取消没有结束传输")
	}
	if s.Catalog.MediaLimits.Stats().Active != 0 {
		t.Fatal("传输额度未释放")
	}
}

type contextMediaBody struct {
	ctx     context.Context
	entered chan struct{}
}

func (b *contextMediaBody) Read([]byte) (int, error) {
	close(b.entered)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *contextMediaBody) Close() error { return nil }

func TestProxyCopyPreservesSuccessfulBody(t *testing.T) {
	s, user, info := newMediaStabilityServer(t, recoveryScript)
	s.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {"bytes 0-5/6"}}, Body: io.NopCloser(strings.NewReader("音频")), Request: r}, nil
	})}
	w := httptest.NewRecorder()
	s.stream(w, mediaStabilityRequest(user, info, "&proxy=1"))
	if w.Code != 206 || w.Body.String() != "音频" || s.Catalog.MediaLimits.Stats().Active != 0 {
		t.Fatal("正常传输或额度回收失败")
	}
}
