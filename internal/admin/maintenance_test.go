package admin

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaintenanceRequiresConfirmationAndSerializesOperations(t *testing.T) {
	s := newAdminStabilityServer(t)
	compact := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.compactMetadata(w, httptest.NewRequest("POST", "/metadata/compact", strings.NewReader(body)))
		return w
	}
	for _, body := range []string{"", "{}", `{"confirm":false}`, "bad"} {
		if w := compact(body); w.Code != 400 {
			t.Fatal("压缩需要明确确认", w.Code)
		}
	}
	s.maintenance.Store(true)
	if w := compact(`{"confirm":true}`); w.Code != 409 {
		t.Fatal("不能并发执行压缩")
	}
	w := httptest.NewRecorder()
	s.cleanupMetadata(w, httptest.NewRequest("POST", "/metadata/cleanup", nil))
	if w.Code != 409 {
		t.Fatal("清理与压缩必须互斥")
	}
	s.maintenance.Store(false)
	w = httptest.NewRecorder()
	s.cleanupMetadata(w, httptest.NewRequest("POST", "/metadata/cleanup", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cleanup"`) || s.maintenance.Load() {
		t.Fatal("清理失败或没有释放维护状态", w.Code)
	}
	if w := compact(`{"confirm":true}`); w.Code != 200 || s.maintenance.Load() {
		t.Fatal("显式压缩失败或没有释放维护状态", w.Code)
	}
}
