package diagnostics

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestProbeReportsNonAudioWithoutReadingOrRecovering(t *testing.T) {
	for _, contentType := range []string{"text/html; secret=NEVER_EXPORT", "application/problem+json"} {
		t.Run(contentType, func(t *testing.T) {
			f := newFixture(t)
			setupProbe(t, f)
			_, key := f.create(t, true)
			body := &unreadBody{t: t}
			calls := 0
			f.s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: body, Request: r}, nil
			})}
			w := f.call("POST", "/api/debug/probe", `{"trackId":"tr-wy-1","quality":"320k"}`, key, nil)
			var result struct {
				Events []Event `json:"events"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Events) != 3 || calls != 1 || !body.closed {
				t.Fatal("探测必须仍只检查一次响应头，不自动刷新或切源")
			}
			last := result.Events[2]
			if last.Stage != "probe" || last.Status != 200 || last.Error != "non_audio" || last.Client != "" || last.Method != "" {
				t.Fatalf("不能把 HTTP 200 错误页标为成功或客户端请求: %+v", last)
			}
			if strings.Contains(w.Body.String(), "NEVER_EXPORT") || strings.Contains(w.Body.String(), "秘密") {
				t.Fatal("探测不得输出原始 MIME 参数或私有内容")
			}
			if events := f.s.Events.List(); events[len(events)-1].Error != "non_audio" {
				t.Fatal("事件白名单必须保留非音频分类")
			}
		})
	}
}
