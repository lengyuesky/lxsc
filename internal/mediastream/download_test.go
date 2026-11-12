package mediastream

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDownloadRangeRequiresMatchingEntityAndOffset(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Range", "bytes=123-")
	r.Header.Set("If-Range", `"file-a"`)
	response := &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Range": {"bytes 123-999/1000"}, "Etag": {`"file-a"`}}}
	if !downloadRangeResponseMatches(r, response) {
		t.Fatal("有效续传被拒绝")
	}
	for _, mutate := range []func(){
		func() { response.Header.Set("ETag", `"file-b"`) },
		func() { response.Header.Del("ETag") },
		func() { response.StatusCode = http.StatusOK },
		func() {
			response.StatusCode = http.StatusPartialContent
			response.Header.Set("ETag", `"file-a"`)
			response.Header.Set("Content-Range", "bytes 124-999/1000")
		},
	} {
		mutate()
		if downloadRangeResponseMatches(r, response) {
			t.Fatal("失效实体、忽略范围或错误片段被当作可续传")
		}
	}
	response.Header.Set("ETag", `"file-a"`)
	for _, value := range []string{"bytes 123-199/1000", "bytes 123-text/1000", "bytes 123-999/*", "bytes 123-1000/1000"} {
		response.Header.Set("Content-Range", value)
		if downloadRangeResponseMatches(r, response) {
			t.Fatal("不完整或非法区间被当作完整剩余文件", value)
		}
	}
}
