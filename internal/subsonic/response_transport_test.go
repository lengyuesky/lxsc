package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"lxsc/internal/diagnostics"
	"lxsc/internal/httpguard"
)

// 正文生成之后的收尾被延迟时，客户端也应能识别并读完已完成的响应。
// 只有 Write 返回全部字节而没有完整的 HTTP 消息边界，ReadAll 仍会一直等待。
func TestProtocolResponseReadableBeforeHandlerCleanup(t *testing.T) {
	for _, count := range []int{1, 200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			entries := make([]M, count)
			for i := range entries {
				entries[i] = M{"id": fmt.Sprintf("tr-wy-%d", i), "title": strings.Repeat("测试歌曲", 12)}
			}
			written, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeOK(w, r, "playlist", M{"entry": entries})
				close(written)
				<-release
			}))
			defer server.Close()
			defer unblock()
			type result struct {
				body []byte
				err  error
			}
			received := make(chan result, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodGet, server.URL+"/rest/getPlaylist?f=json", nil)
				req.Header.Set("Accept-Encoding", "identity")
				resp, err := server.Client().Do(req)
				if err != nil {
					received <- result{err: err}
					return
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				received <- result{body, err}
			}()
			select {
			case <-written:
			case <-time.After(3 * time.Second):
				t.Fatal("服务端未完成正文写入")
			}
			select {
			case got := <-received:
				var decoded struct {
					Response struct {
						Status   string `json:"status"`
						Playlist struct {
							Entries []M `json:"entry"`
						} `json:"playlist"`
					} `json:"subsonic-response"`
				}
				if got.err != nil || json.Unmarshal(got.body, &decoded) != nil || decoded.Response.Status != "ok" || len(decoded.Response.Playlist.Entries) != count {
					t.Fatalf("客户端没有读到完整歌单: bytes=%d err=%v", len(got.body), got.err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Error("正文已经写完，客户端仍在等待响应结束，可能超时并显示空白歌单")
				unblock()
				<-received
			}
		})
	}
}

type protocolFlushWriter struct {
	*httptest.ResponseRecorder
	err     error
	flushes int
	onFlush func()
}

func (w *protocolFlushWriter) FlushError() error {
	w.flushes++
	if w.onFlush != nil {
		w.onFlush()
	}
	return w.err
}

func TestProtocolResponseFlushFailureIsNotSuccess(t *testing.T) {
	s := &Server{Diagnostics: &diagnostics.Events{}}
	req := httptest.NewRequest(http.MethodGet, "/rest/getPlaylist?f=json&c=Amcfy", nil)
	req, finish := s.beginClientDiagnostic(req, "getPlaylist")
	pending := req.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
	writer := &protocolFlushWriter{ResponseRecorder: httptest.NewRecorder(), err: errors.New("PRIVATE connection reset"), onFlush: func() {
		if pending.Result != "unavailable" {
			t.Error("网络刷新完成前已记录成功")
		}
	}}
	writeOK(writer, req, "playlist", M{"entry": []M{{"id": "tr-wy-one"}}})
	finish()
	event := s.Diagnostics.List()[0]
	if writer.flushes != 1 || event.Result != "failed" || event.Error != "write_error" {
		t.Fatalf("正文进入缓冲区后发送失败，不能被记录为成功: flushes=%d event=%+v", writer.flushes, event)
	}
	if event.BytesWritten == nil || event.ResponseBytes == nil || *event.BytesWritten != *event.ResponseBytes {
		t.Fatalf("刷新失败仍应保留已被接受的字节数: %+v", event)
	}
}

func TestProtocolResponsesWithSlowReadsAndConnectionReuse(t *testing.T) {
	entries := make([]M, 200)
	for i := range entries {
		entries[i] = M{"id": fmt.Sprintf("tr-wy-%03d", i), "title": strings.Repeat("歌曲与歌手", 30)}
	}
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", http2), func(t *testing.T) {
			s := &Server{Diagnostics: &diagnostics.Events{}}
			completed := make(chan struct{}, 1)
			handler := httpguard.LimitIO(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r, finish := s.beginClientDiagnostic(r, "getPlaylist")
				defer finish()
				w.Header().Set("Cache-Control", "no-store")
				writeOK(w, r, "playlist", M{"entry": entries})
			}))
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(w, r)
				completed <- struct{}{}
			}))
			server.EnableHTTP2 = http2
			server.StartTLS()
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			requestCount := 0
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				for _, format := range []string{"json", "xml", "jsonp"} {
					target := server.URL + "/rest/getPlaylist?f=" + format + "&callback=render&c=Amcfy"
					req, err := http.NewRequest(method, target, nil)
					if err != nil {
						t.Fatal(err)
					}
					reused := false
					req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
					req.Header.Set("Accept-Encoding", "identity")
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					var body strings.Builder
					buf := make([]byte, 4096)
					for {
						n, readErr := resp.Body.Read(buf)
						body.Write(buf[:n])
						if readErr != nil {
							if readErr != io.EOF {
								t.Errorf("慢速读取时响应被截断: %v", readErr)
							}
							break
						}
						time.Sleep(time.Millisecond)
					}
					resp.Body.Close()
					<-completed
					if resp.StatusCode != 200 || resp.ContentLength != int64(body.Len()) || len(resp.TransferEncoding) != 0 || resp.Header.Get("Cache-Control") != "no-store" {
						t.Fatalf("响应边界或状态错误: method=%s format=%s status=%d length=%d bytes=%d encoding=%v", method, format, resp.StatusCode, resp.ContentLength, body.Len(), resp.TransferEncoding)
					}
					if (resp.ProtoMajor == 2) != http2 || (requestCount > 0 && !reused) {
						t.Fatalf("没有验证预期协议的连接复用: proto=%s reused=%v", resp.Proto, reused)
					}
					text := body.String()
					var got []M
					if format == "xml" {
						var decoded struct {
							Playlist struct {
								Entries []struct {
									ID string `xml:"id,attr"`
								} `xml:"entry"`
							} `xml:"playlist"`
						}
						if err := xml.Unmarshal([]byte(text), &decoded); err != nil {
							t.Fatal(err)
						}
						for _, entry := range decoded.Playlist.Entries {
							got = append(got, M{"id": entry.ID})
						}
					} else {
						if format == "jsonp" {
							text = strings.TrimSuffix(strings.TrimPrefix(text, "render("), ");")
						}
						var decoded struct {
							Response struct {
								Playlist struct {
									Entries []M `json:"entry"`
								} `json:"playlist"`
							} `json:"subsonic-response"`
						}
						if err := json.Unmarshal([]byte(text), &decoded); err != nil {
							t.Fatal(err)
						}
						got = decoded.Response.Playlist.Entries
					}
					if len(got) != len(entries) {
						t.Fatalf("歌单歌曲不完整: got=%d want=%d", len(got), len(entries))
					}
					for i := range entries {
						if got[i]["id"] != entries[i]["id"] {
							t.Fatalf("歌曲顺序发生变化: index=%d got=%v", i, got[i])
						}
					}
					requestCount++
					events := s.Diagnostics.List()
					event := events[len(events)-1]
					if event.Result != "ok" || event.Error != "none" || event.RequestID != resp.Header.Get("X-Request-ID") || event.Count == nil || *event.Count != len(entries) || event.BytesWritten == nil || *event.BytesWritten != int64(body.Len()) {
						t.Fatalf("实际收到的响应与诊断不一致: %+v", event)
					}
				}
			}
		})
	}
}
