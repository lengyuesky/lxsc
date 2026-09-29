package portal

import (
	"lxsc/internal/admission"
	"testing"
)

func TestSearchRejectsOverloadAndRecovers(t *testing.T) {
	f := newPortalFixture(t)
	f.service.Catalog.RequestLimits = admission.New(1, 0, 1, 0)
	permit, _ := f.service.Catalog.RequestLimits.Reserve(f.users["alice"].ID)
	client := f.client(t, "alice")
	for _, path := range []string{"/search", "/search/stream"} {
		status, body := doJSON(t, client, "POST", f.server.URL+path, map[string]any{"query": "歌曲"})
		if status != 503 || body["error"] != admission.ErrBusy.Error() {
			t.Fatalf("过载必须明确返回 503: %d %+v", status, body)
		}
	}
	permit.Release()
	status, _ := doJSON(t, client, "POST", f.server.URL+"/search", map[string]any{"query": "歌曲"})
	if status != 200 {
		t.Fatalf("额度释放后应恢复服务: %d", status)
	}
}
