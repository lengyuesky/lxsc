package diagnostics

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/logbuf"
)

func fullCredential(t *testing.T, f *fixture) (TokenView, string) {
	t.Helper()
	view, key, err := f.s.Tokens.CreateScoped(f.admin.ID, 900, MaximumScopes())
	if err != nil {
		t.Fatal(err)
	}
	return view, key
}

func TestCoverPerformanceInspectionIncludesSeparateAdmission(t *testing.T) {
	f := newFixture(t)
	_, key := fullCredential(t, f)
	f.s.CoverPerformance = func() any {
		return map[string]any{"admission": map[string]int{"active": 4, "queued": 2, "rejected": 3}, "cacheBytes": 2 << 20, "placeholders": 1}
	}
	w := f.call("GET", "/api/debug/inspect/performance", "", key, nil)
	var response struct {
		Data struct {
			Covers struct {
				Admission    struct{ Active, Queued, Rejected int }
				CacheBytes   int
				Placeholders int
			}
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Data.Covers.Admission.Rejected != 3 || response.Data.Covers.CacheBytes != 2<<20 || response.Data.Covers.Placeholders != 1 {
		t.Fatalf("独立封面并发和缓存未进入诊断：%d %s", w.Code, w.Body.String())
	}
}

func TestMaximumDebugDefaultAndAdminScopes(t *testing.T) {
	f := newFixture(t)
	w := f.call("POST", "/api/admin/debug-tokens", "{}", "", f.cookie)
	var created struct {
		Token      string
		Credential TokenView
	}
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &created) != nil || !reflect.DeepEqual(created.Credential.Scopes, MaximumScopes()) {
		t.Fatalf("管理员新建应默认为最大调试权限: %d %s", w.Code, w.Body)
	}
	_, read := f.create(t, false)
	_, probe := f.create(t, true)
	for _, endpoint := range []string{"runtime", "performance", "settings", "sources", "logs"} {
		path := "/api/debug/inspect/" + endpoint
		for _, key := range []string{read, probe} {
			if w := f.call("GET", path, "", key, nil); w.Code != 403 {
				t.Fatalf("旧权限不能自动提权: %s %d", path, w.Code)
			}
		}
		if w := f.call("GET", path, "", created.Token, nil); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("完整调试应可查看: %s %d", path, w.Code)
		}
		if w := f.call("GET", path, "", "", f.normalCookie); w.Code != 401 {
			t.Fatal("普通会话不能访问调试", path, w.Code)
		}
	}
	_, normalKey, err := f.s.Tokens.CreateScoped(f.normal.ID, 900, MaximumScopes())
	if err != nil {
		t.Fatal(err)
	}
	if w := f.call("GET", "/api/debug/inspect/runtime", "", normalKey, nil); w.Code != 401 {
		t.Fatal("即使内部生成凭据，普通所有者也不能调试", w.Code)
	}
	for _, scopes := range [][]string{{"read", "maintain"}, {"read", "inspect", "inspect"}, {"read", "admin"}} {
		if _, _, err := f.s.Tokens.CreateScoped(f.admin.ID, 900, scopes); err == nil {
			t.Fatal("非法权限组合被接受", scopes)
		}
	}
}

func TestFullInspectionRedactsWithoutChangingLogs(t *testing.T) {
	f := newFixture(t)
	_, key := fullCredential(t, f)
	f.s.Logs = logbuf.New(1000)
	message := `音源请求 Authorization=Bearer PRIVATE_BEARER password="PRIVATE_PASSWORD" apiKey=PRIVATE_API_KEY https://user:PRIVATE_PROXY_PASSWORD@cdn.example/private/path?signature=PRIVATE_SIGNATURE`
	f.s.Logs.Add(logbuf.Entry{Time: "2026-01-01 00:00:00", Level: "WARN", Message: message})
	f.s.Logs.Add(logbuf.Entry{Time: "2026-01-01 00:00:01", Level: "INFO", Message: "普通信息"})
	w := f.call("POST", "/api/debug/inspect/logs", `{"limit":1,"level":"WARN","contains":"音源"}`, key, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "cdn.example") || strings.Contains(w.Body.String(), "PRIVATE_") || strings.Contains(w.Body.String(), "private/path") || strings.Contains(w.Body.String(), "普通信息") {
		t.Fatal("日志筛选或已知凭据隐藏错误", w.Code, w.Body)
	}
	if f.s.Logs.List(100)[0].Message != message {
		t.Fatal("读取调试日志不能修改原缓冲")
	}
	for _, address := range []string{"socks5://user:PRIVATE_PROXY_PASSWORD@proxy.example:1080/path?token=PRIVATE_TOKEN", "https://user:PRIVATE_PROXY_PASSWORD@proxy.example/private"} {
		raw, _ := json.Marshal(RedactValue(map[string]any{"address": address, "summary": AddressSummary(address)}))
		if strings.Contains(string(raw), "PRIVATE_") || !strings.Contains(string(raw), "proxy.example") {
			t.Fatal("代理摘要或日志包含凭据", string(raw))
		}
	}
	f.s.RuntimeInfo = map[string]any{"secret_key": "PRIVATE_RUNTIME_SECRET", "proxy": "https://user:PRIVATE_PROXY_PASSWORD@proxy.example/private?token=PRIVATE_TOKEN"}
	w = f.call("GET", "/api/debug/inspect/runtime", "", key, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE_") {
		t.Fatal("运行摘要泄露凭据", w.Body)
	}
	for _, body := range []string{`{"limit":0}`, `{"limit":1001}`, `{"level":"invalid"}`, `{"unknown":true}`} {
		if w := f.call("POST", "/api/debug/inspect/logs", body, key, nil); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
}

func TestFullSourceInspectionAndReload(t *testing.T) {
	f := newFixture(t)
	setupProbe(t, f)
	f.s.Sources = f.s.Catalog.Sources
	_, key := fullCredential(t, f)
	script := `// @name 合成音源
lx.on(lx.EVENT_NAMES.request,()=>Promise.resolve('https://media.example/file?token=PRIVATE_SOURCE_TOKEN'));
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{name:'合成',type:'music',actions:['musicUrl'],qualitys:['320k']}}});`
	source, err := f.s.DB.CreateSource(context.Background(), &db.Source{Name: "合成音源", Script: script, Enabled: true, Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	w := f.call("GET", "/api/debug/inspect/sources", "", key, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "scriptSHA256") || strings.Contains(w.Body.String(), "PRIVATE_SOURCE_TOKEN") || strings.Contains(w.Body.String(), "lx.on") {
		t.Fatal("音源摘要错误", w.Code, w.Body)
	}
	w = f.call("POST", "/api/debug/maintenance", `{"operation":"source_reload","sourceId":`+jsonNumber(source.ID)+`}`, key, nil)
	if w.Code != 200 || f.s.Sources.StatusOf(source.ID).State != "ready" {
		t.Fatal("音源重载失败", w.Code, w.Body)
	}
	stored, err := f.s.DB.GetSource(context.Background(), source.ID)
	if err != nil || stored.Script != script {
		t.Fatal("重载不能改写脚本", err)
	}
}

func TestMaintenanceScopeValidationAndAudit(t *testing.T) {
	f := newFixture(t)
	view, full := fullCredential(t, f)
	_, read := f.create(t, false)
	_, diagnose, err := f.s.Tokens.CreateScoped(f.admin.ID, 900, []string{"read", "probe", "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"operation":"settings_update","settings":{"streamMode":"proxy"}}`
	for _, key := range []string{read, diagnose} {
		if w := f.call("POST", "/api/debug/maintenance", body, key, nil); w.Code != 403 {
			t.Fatal("维护必须单独授权", w.Code)
		}
	}
	if f.s.Settings.Get().StreamMode != "redirect" {
		t.Fatal("拒绝请求修改了设置")
	}
	if w := f.call("POST", "/api/debug/maintenance", body, full, nil); w.Code != 200 || f.s.Settings.Get().StreamMode != "proxy" {
		t.Fatal(w.Code, w.Body)
	}
	if w := f.call("POST", "/api/debug/maintenance", `{"operation":"cache_clear","target":"all"}`, full, nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	_, audit := f.s.Tokens.List()
	operations := []string{}
	for _, entry := range audit {
		if entry.Action == "maintenance" {
			if entry.ID != view.ID {
				t.Fatal("审计归属错误")
			}
			operations = append(operations, entry.Operation)
		}
	}
	if !reflect.DeepEqual(operations, []string{"settings_update", "cache_clear"}) {
		t.Fatal("缺少维护审计", operations)
	}
	for _, invalid := range []string{
		`{"operation":"users_delete","sourceId":1}`,
		`{"operation":"cache_clear","target":"all","settings":{}}`,
		`{"operation":"settings_update","settings":{"admin_password":"PRIVATE"}}`,
		`{"operation":"settings_update","settings":{"streamMode":"invalid"}}`,
		`{"operation":"settings_update","settings":{"searchLimit":10000}}`,
		`{"operation":"settings_update","settings":{"showBoards":"yes"}}`,
		`{"operation":"settings_update","settings":{"searchSources":["wy","unknown"]}}`,
	} {
		if w := f.call("POST", "/api/debug/maintenance", invalid, full, nil); w.Code != 400 {
			t.Fatal("无效维护请求被接受", invalid, w.Code)
		}
	}
	f.s.Tokens.Revoke(view.ID)
	if w := f.call("POST", "/api/debug/maintenance", body, full, nil); w.Code != 401 {
		t.Fatal("撤销后仍可维护", w.Code)
	}
}

func TestFullProtocolAdminDelegationLimitsAndRevocation(t *testing.T) {
	f := newFixture(t)
	view, full := fullCredential(t, f)
	_, probe := f.create(t, true)
	f.s.ProtocolEndpoints = []string{"getSong"}
	calls := 0
	f.s.ProtocolProbe = func(ctx context.Context, user *db.User, req ProtocolRequest) (ProtocolResult, error) {
		calls++
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) < 45*time.Second || user.ID != f.normal.ID || req.Endpoint != "getSong" {
			t.Error("完整权限预算或模拟用户错误")
		}
		return ProtocolResult{Status: 200, Format: "xml", ProtocolStatus: "failed", ProtocolCode: new(int)}, nil
	}
	body := `{"endpoint":"getSong","method":"GET","format":"xml","userId":` + jsonNumber(f.normal.ID) + `,"params":{"id":"tr-wy-1"}}`
	if w := f.call("POST", "/api/debug/probe/protocol", body, probe, nil); w.Code != 403 || calls != 0 {
		t.Fatal("基础探测不能模拟用户", w.Code, calls)
	}
	w := f.call("POST", "/api/debug/probe/protocol", body, full, nil)
	if w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, w.Body, calls)
	}
	last := f.s.Events.List()[0]
	if last.Stage != "protocol_probe" || last.Result != "failed" || last.Format != "xml" || last.ProtocolCode == nil {
		t.Fatal("不能把 XML 协议失败或主动探测误记为客户端成功", last)
	}
	for _, body := range []string{`{"endpoint":"deletePlaylist","params":{}}`, `{"endpoint":"getSong","params":{"apiKey":"PRIVATE"}}`} {
		if w := f.call("POST", "/api/debug/probe/protocol", body, full, nil); w.Code != 400 || calls != 1 {
			t.Fatal("禁止越权操作和真实认证参数", w.Code, calls)
		}
	}
	entered := make(chan struct{})
	f.s.ProtocolProbe = func(ctx context.Context, _ *db.User, _ ProtocolRequest) (ProtocolResult, error) {
		close(entered)
		<-ctx.Done()
		return ProtocolResult{Status: 200, Body: "PRIVATE_RESULT"}, nil
	}
	done := make(chan string, 1)
	go func() {
		response := f.call("POST", "/api/debug/probe/protocol", `{"endpoint":"getSong","params":{}}`, full, nil)
		done <- response.Body.String()
	}()
	<-entered
	f.s.Tokens.Revoke(view.ID)
	select {
	case result := <-done:
		if strings.Contains(result, "PRIVATE_RESULT") || !strings.Contains(result, "cancelled") {
			t.Fatal("撤销后仍返回探测内容", result)
		}
	case <-time.After(time.Second):
		t.Fatal("撤销未取消完整诊断")
	}
}

func jsonNumber(value int64) string { data, _ := json.Marshal(value); return string(data) }

func TestFullEventWindowAndRate(t *testing.T) {
	f := newFixture(t)
	_, full := fullCredential(t, f)
	_, read := f.create(t, false)
	for range 300 {
		f.s.Events.Add(Event{Stage: "metadata", Error: "none"})
	}
	for _, tc := range []struct {
		key  string
		want int
	}{{read, 200}, {full, 300}} {
		w := f.call("GET", "/api/debug/events", "", tc.key, nil)
		var result struct{ Events []Event }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Events) != tc.want {
			t.Fatal("事件窗口未按权限隔离", w.Code, len(result.Events))
		}
	}
	for range 119 {
		if w := f.call("GET", "/api/debug/status", "", full, nil); w.Code != 200 {
			t.Fatal("完整诊断应支持每分钟120次", w.Code)
		}
	}
	if w := f.call("GET", "/api/debug/status", "", full, nil); w.Code != http.StatusTooManyRequests {
		t.Fatal("仍须有界限频", w.Code)
	}
}
