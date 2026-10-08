package httpguard

import (
	"errors"
	"net/http"
	"testing"
)

func TestMediaResponseHeaderValidation(t *testing.T) {
	for _, tc := range []struct {
		name, contentType string
		status            int
		rejected          bool
	}{
		{"HTML错误页", "text/html; charset=utf-8", 200, true},
		{"分段JSON错误", "application/json; secret=PRIVATE", 206, true},
		{"带参数错误的HTML", "Text/HTML; invalid", 200, true},
		{"文本错误", "text/plain", 200, true},
		{"结构化错误", "application/problem+json", 200, true},
		{"XML错误", "application/xml", 200, true},
		{"XHTML错误", "application/xhtml+xml", 200, true},
		{"错误图片", "image/png", 200, true},
		{"MP3", "audio/mpeg", 200, false},
		{"FLAC", "audio/flac", 206, false},
		{"通用二进制", "application/octet-stream", 206, false},
		{"未声明类型", "", 200, false},
		{"MP4音频容器", "video/mp4", 200, false},
		{"范围错误保留HTTP状态", "text/html", 416, false},
		{"过期链接保留刷新规则", "text/html", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 没有 Body，检查不能依赖读取音频或错误页正文。
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {tc.contentType}}}
			if err := CheckMediaResponse(resp); errors.Is(err, ErrNonAudioResponse) != tc.rejected {
				t.Fatalf("媒体响应判断错误: status=%d err=%v", tc.status, err)
			}
		})
	}
}
