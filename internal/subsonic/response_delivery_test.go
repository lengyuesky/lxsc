package subsonic

import (
	"encoding/json"
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"lxsc/internal/diagnostics"
)

type deliveryTestWriter struct {
	*httptest.ResponseRecorder
	limit   int
	err     error
	writes  int
	onWrite func()
}

func (w *deliveryTestWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.onWrite != nil {
		w.onWrite()
	}
	n := len(data)
	if w.limit >= 0 {
		n = min(n, w.limit)
	}
	w.ResponseRecorder.Write(data[:n])
	return n, w.err
}

func TestResponseDeliveryCountsAndFailures(t *testing.T) {
	for _, format := range []string{"json", "jsonp", "xml"} {
		for _, tc := range []struct {
			name  string
			limit int
			err   error
		}{
			{"complete", -1, nil},
			{"short_write", 11, nil},
			{"write_error", 7, errors.New("PRIVATE transport detail")},
			{"error_after_full_write", -1, errors.New("PRIVATE transport detail")},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				s := &Server{Diagnostics: &diagnostics.Events{}}
				req := httptest.NewRequest("GET", "/rest/getPlaylist?id=lb-wy-one&f="+format+"&callback=receive&c=Amcfy", nil)
				req.Header.Set("X-Request-ID", "PRIVATE client supplied ID")
				req, finish := s.beginClientDiagnostic(req, "getPlaylist")
				pending := req.Context().Value(clientDiagnosticKey{}).(*diagnostics.Event)
				writer := &deliveryTestWriter{ResponseRecorder: httptest.NewRecorder(), limit: tc.limit, err: tc.err, onWrite: func() {
					if pending.Result != "unavailable" {
						t.Fatal("写出完成前就已记录成功")
					}
				}}
				writeOK(writer, req, "playlist", M{"entry": []M{{"id": "tr-wy-one", "title": "PRIVATE song title"}}})
				finish()
				event := s.Diagnostics.List()[0]
				if event.RequestID == "" || event.RequestID != writer.Header().Get("X-Request-ID") || event.BytesWritten == nil || *event.BytesWritten != int64(writer.Body.Len()) || event.ResponseBytes == nil {
					t.Fatalf("缺少请求关联 ID 或实际写出计数: %+v", event)
				}
				if event.Count == nil || *event.Count != 1 || event.Format != format || writer.writes != 1 {
					t.Fatalf("协议正文应一次写出，保留条目数量与格式: %+v", event)
				}
				if tc.name == "complete" {
					if event.Result != "ok" || event.Error != "none" || *event.BytesWritten != *event.ResponseBytes {
						t.Fatalf("正常写出记录错误: %+v", event)
					}
				} else if event.Result != "failed" || event.Error != "write_error" {
					t.Fatalf("写出失败不能记录成功: %+v", event)
				}
				if tc.limit >= 0 && *event.ResponseBytes <= *event.BytesWritten {
					t.Fatalf("短写没有保留预期长度: %+v", event)
				}
				raw, err := json.Marshal(event)
				if err != nil || strings.Contains(string(raw), "PRIVATE") {
					t.Fatalf("诊断不得包含正文、请求头或原始写出错误: %s %v", raw, err)
				}
			})
		}
	}
}

func TestResponseEncodingFailureReturnsProtocolError(t *testing.T) {
	for _, format := range []string{"json", "jsonp"} {
		t.Run(format, func(t *testing.T) {
			s := &Server{Diagnostics: &diagnostics.Events{}}
			req := httptest.NewRequest("GET", "/rest/getPlaylist?f="+format+"&callback=receive", nil)
			req, finish := s.beginClientDiagnostic(req, "getPlaylist")
			rec := httptest.NewRecorder()
			writeOK(rec, req, "playlist", M{"invalid": math.NaN(), "title": "PRIVATE"})
			finish()
			event := s.Diagnostics.List()[0]
			if rec.Code != 500 || event.Status != 500 || event.Result != "failed" || event.Error != "encode_error" || event.ProtocolCode == nil || *event.ProtocolCode != ErrGeneric || event.Count != nil {
				t.Fatalf("序列化失败被错误记录或错误返回: %+v %d", event, rec.Code)
			}
			if event.BytesWritten == nil || *event.BytesWritten != int64(rec.Body.Len()) || event.ResponseBytes == nil || *event.ResponseBytes != *event.BytesWritten {
				t.Fatalf("错误响应的实际写出长度不正确: %+v", event)
			}
			body := rec.Body.String()
			if format == "jsonp" {
				body = strings.TrimSuffix(strings.TrimPrefix(body, "receive("), ");")
			}
			var decoded map[string]map[string]any
			if err := json.Unmarshal([]byte(body), &decoded); err != nil || decoded["subsonic-response"]["status"] != "failed" || strings.Contains(body, "PRIVATE") {
				t.Fatalf("必须返回合法且不含原 payload 的协议错误: %s %v", body, err)
			}
		})
	}
}

func TestResponseHeadErrorsKeepStatusAndNoBody(t *testing.T) {
	for _, tc := range []struct {
		code, status int
		retry        string
	}{
		{ErrGeneric, 502, ""}, {ErrMissingParam, 400, ""}, {ErrWrongAuth, 401, ""},
		{ErrBusy, 503, "2"}, {ErrUnavailable, 503, "2"}, {ErrAuthLimited, 429, "60"},
	} {
		s := &Server{Diagnostics: &diagnostics.Events{}}
		req := httptest.NewRequest("HEAD", "/rest/stream?f=json", nil)
		req, finish := s.beginClientDiagnostic(req, "stream")
		writer := &deliveryTestWriter{ResponseRecorder: httptest.NewRecorder(), limit: -1}
		writeErr(writer, req, tc.code, "暂时不可用")
		finish()
		event := s.Diagnostics.List()[0]
		if writer.Code != tc.status || writer.writes != 0 || writer.Header().Get("Retry-After") != tc.retry || writer.Header().Get("Cache-Control") != "no-store" || event.Result != "failed" || event.Status != tc.status || event.BytesWritten == nil || *event.BytesWritten != 0 {
			t.Fatalf("HEAD 错误行为回归: code=%d event=%+v headers=%v", tc.code, event, writer.Header())
		}
	}
}
