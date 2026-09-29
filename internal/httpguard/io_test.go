package httpguard

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSlowBodyReadIsBounded(t *testing.T) {
	done := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		done <- err
		http.Error(w, "读取超时", 408)
	})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWithBudget(handler, w, r, ioBudget{read: 50 * time.Millisecond, write: time.Second, total: time.Second, body: 1024})
	}))
	defer s.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 10\r\n\r\na")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("慢正文不应完整读取")
		}
	case <-time.After(time.Second):
		t.Fatal("正文读取未受截止时间限制")
	}
}

func TestBodyLimitAndMediaBudget(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/admin/users", strings.NewReader("too large"))
	w := httptest.NewRecorder()
	serveWithBudget(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		var limit *http.MaxBytesError
		if !errors.As(err, &limit) {
			t.Error("正文大小未限制", err)
		}
	}), w, r, ioBudget{read: time.Second, write: time.Second, total: time.Second, body: 2})
	for _, path := range []string{"/rest/stream.view", "/rest/download", "/api/app/stream"} {
		if requestBudget(httptest.NewRequest("GET", path, nil)).total != 0 {
			t.Fatal("正常音频不能受普通请求总期限截断")
		}
	}
}

type stalledBody struct {
	done chan struct{}
	once sync.Once
}

func (b *stalledBody) Read([]byte) (int, error) { <-b.done; return 0, io.EOF }
func (b *stalledBody) Close() error             { b.once.Do(func() { close(b.done) }); return nil }

func TestCopyIdleClosesStalledUpstream(t *testing.T) {
	body := &stalledBody{done: make(chan struct{})}
	start := time.Now()
	_, err := CopyIdle(httptest.NewRecorder(), body, 30*time.Millisecond)
	if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("上游空闲未被中止", err)
	}
}

func TestCopyIdleBoundsSlowClient(t *testing.T) {
	done := make(chan error, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := CopyIdle(w, io.NopCloser(io.LimitReader(repeatReader{}, 64<<20)), 50*time.Millisecond)
		done <- err
	}))
	defer s.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("不读响应的客户端应触发写超时")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("下游写入没有超时")
	}
}

func TestLimitIODoesNotTruncateChunkedResponses(t *testing.T) {
	payload := strings.Repeat("合成响应", 1024)
	s := httptest.NewServer(LimitIO(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	})))
	defer s.Close()
	for range 50 {
		resp, err := s.Client().Get(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || string(body) != payload {
			t.Fatal("分块响应不能因结束读取而被截断", err, len(body))
		}
	}
}

type repeatReader struct{}

func (repeatReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
