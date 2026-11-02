package subsonic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lxsc/internal/admission"
	"lxsc/internal/coverart"
	"lxsc/internal/diagnostics"
)

type blockedCoverFlush struct {
	*httptest.ResponseRecorder
	started chan struct{}
	release chan struct{}
}

func (w *blockedCoverFlush) FlushError() error {
	close(w.started)
	<-w.release
	return nil
}

func TestCoverSlowDeliveryKeepsSeparateLimitUntilFlushed(t *testing.T) {
	f, router, tracks := burstCoverFixture(t, 1)
	picture := customCoverPNG(t)
	f.server.Covers.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	f.server.initCoverClient()
	f.server.Covers.Delivery = admission.New(1, 0, 0, 0)
	w := &blockedCoverFlush{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-w.release:
		default:
			close(w.release)
		}
	}()
	done := make(chan struct{})
	go func() {
		router.ServeHTTP(w, httptest.NewRequest("GET", "/rest/getCoverArt?apiKey=cover-burst-key&id="+tracks[0].TrackID(), nil))
		close(done)
	}()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("未进入图片刷新阶段")
	}
	if f.server.Covers.Limits.Stats().Active != 0 || f.server.Covers.Delivery.Stats().Active != 1 {
		t.Error("下载结束后应仅占用大图写出额度")
	}
	second := coverClientRequest(router, tracks[0].TrackID())
	if second.Code != 200 || bytes.Equal(second.Body.Bytes(), picture) {
		t.Error("传输已满时应返回小型默认图")
	}
	close(w.release)
	<-done
	if f.server.Covers.Delivery.Stats().Active != 0 {
		t.Fatal("刷新结束后未归还写出额度")
	}
}

func TestCoverFlushFailureOverridesSuccessfulOrPlaceholderImage(t *testing.T) {
	for _, placeholder := range []bool{false, true} {
		f, router, tracks := burstCoverFixture(t, 1)
		picture := customCoverPNG(t)
		f.server.Covers.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			status := 200
			if placeholder {
				status = 503
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
		})}
		w := &protocolFlushWriter{ResponseRecorder: httptest.NewRecorder(), err: errors.New("不应记录的网络错误正文")}
		router.ServeHTTP(w, httptest.NewRequest("GET", "/rest/getCoverArt?apiKey=cover-burst-key&id="+tracks[0].TrackID(), nil))
		events := f.server.Diagnostics.List()
		last := events[len(events)-1]
		if last.Result != "failed" || last.Error != "write_error" || last.BytesWritten == nil || last.ResponseBytes == nil || *last.BytesWritten != *last.ResponseBytes {
			t.Fatalf("刷新失败仍被记录为成功：%+v", last)
		}
	}
}

func TestCoverProbeReportsPlaceholderAndSharesCache(t *testing.T) {
	f, router, tracks := burstCoverFixture(t, 1)
	picture := customCoverPNG(t)
	calls := 0
	f.server.Covers.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	if w := coverClientRequest(router, tracks[0].TrackID()); w.Code != 200 {
		t.Fatal(w.Code)
	}
	req := diagnostics.ProtocolRequest{Endpoint: "getCoverArt", Method: "GET", Format: "json", Params: map[string]string{"id": tracks[0].TrackID()}}
	result, err := f.server.ProbeProtocol(context.Background(), f.user, req)
	if err != nil || !result.Cached || result.CoverOrigin != "original" || result.Format != "binary" || calls != 1 {
		t.Fatal("主动探测未共享服务端缓存或结果标记错误", result, err)
	}
	f.server.Covers = &coverart.Service{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})}}
	result, err = f.server.ProbeProtocol(context.Background(), f.user, req)
	if err != nil || result.Status != 200 || result.CoverOrigin != "placeholder" || result.ProtocolStatus != "unavailable" || result.Error != "upstream_error" {
		t.Fatal("默认封面被主动探测误报为成功真实封面", result, err)
	}
}
