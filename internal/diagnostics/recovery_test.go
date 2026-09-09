package diagnostics

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestRejectedSlowBodiesFinishTCPConnection(t *testing.T) {
	for _, scenario := range []string{"no_token", "scope", "method", "content_type", "unknown_field", "origin", "rate", "concurrent", "management_auth", "management_csrf"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			_, key := f.create(t, scenario != "scope")
			method, path, contentType, body, extra, want := "POST", "/api/debug/probe", "application/json", "{", "", 403
			switch scenario {
			case "no_token":
				key, want = "", 401
			case "method":
				method, want = "PUT", 405
			case "content_type":
				contentType, want = "text/plain", 400
			case "unknown_field":
				body, want = `{"unknown":`, 400
			case "origin":
				extra = "Origin: https://other.test\r\n"
			case "rate":
				for i := 0; i < callsPerMinute; i++ {
					f.s.Tokens.acquire(key)
				}
				want = 429
			case "concurrent":
				for i := 0; i < cap(f.s.concurrent); i++ {
					f.s.concurrent <- struct{}{}
				}
				want = 429
			case "management_auth":
				path, key, want = "/api/admin/debug-tokens", "", 401
			case "management_csrf":
				path, key = "/api/admin/debug-tokens", ""
				extra = "Cookie: " + f.cookie.String() + "\r\n"
			}
			server := httptest.NewServer(f.router)
			defer server.Close()
			conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, err = fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: debug.test\r\nAuthorization: Bearer %s\r\nContent-Type: %s\r\nContent-Length: 2000\r\n%s\r\n%s", method, path, key, contentType, extra, body)
			if err != nil {
				t.Fatal(err)
			}
			// 不发送其余正文、不主动断开；既验证可读响应，又验证服务端关闭TCP。
			raw, err := io.ReadAll(conn)
			if err != nil {
				t.Fatal("提前拒绝后仍等待未读正文", err)
			}
			if !strings.HasPrefix(string(raw), fmt.Sprintf("HTTP/1.1 %d ", want)) {
				t.Fatalf("响应状态错误: %q", raw)
			}
		})
	}
}

var blockingFunctionID atomic.Uint64

// 仅在合成数据库注册阻塞触发器，真实占用 DB 唯一连接，不增加生产测试后门。
func fixtureWithOccupiedDatabase(t *testing.T) *fixture {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	name := fmt.Sprintf("debug_test_block_%d", blockingFunctionID.Add(1))
	if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		close(started)
		<-release
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	control, err := sql.Open("sqlite", f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec("CREATE TRIGGER debug_block BEFORE UPDATE ON users BEGIN SELECT " + name + "(); END"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- f.s.DB.UpdateUser(context.Background(), f.admin.ID, f.admin.Name, "", true, "320k")
	}()
	t.Cleanup(func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("未占用数据库连接")
	}
	return f
}

func TestOwnerLookupIsInsideConcurrencyAndRevocationBoundary(t *testing.T) {
	f := fixtureWithOccupiedDatabase(t)
	view, key := f.create(t, false)
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- f.call("GET", "/api/debug/status", "", key, nil).Code }()
	}
	until := time.Now().Add(time.Second)
	for len(f.s.concurrent) != 8 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(f.s.concurrent) != 8 {
		t.Fatal("等待数据库的请求没有占用8个并发槽")
	}
	ninth := make(chan int, 1)
	go func() { ninth <- f.call("GET", "/api/debug/status", "", key, nil).Code }()
	select {
	case code := <-ninth:
		if code != 429 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("第9个请求排队等待数据库")
	}
	f.s.Tokens.Revoke(view.ID)
	for i := 0; i < 8; i++ {
		select {
		case code := <-results:
			if code != 408 && code != 401 {
				t.Error(code)
			}
		case <-time.After(time.Second):
			t.Fatal("撤销未取消数据库连接池等待")
		}
	}
	if len(f.s.concurrent) != 0 {
		t.Fatal("数据库等待槽未释放")
	}
}

func TestOwnerLookupExpiryAndAbsoluteRequestTimeout(t *testing.T) {
	for _, scenario := range []string{"expiry", "request_timeout"} {
		t.Run(scenario, func(t *testing.T) {
			f := fixtureWithOccupiedDatabase(t)
			view, key := f.create(t, false)
			if scenario == "expiry" {
				f.s.Tokens.mu.Lock()
				token := f.s.Tokens.entries[view.ID]
				token.ExpiresAt = time.Now().Add(150 * time.Millisecond)
				token.timer.Reset(time.Until(token.ExpiresAt))
				f.s.Tokens.mu.Unlock()
			}
			started := time.Now()
			result := make(chan int, 1)
			go func() { result <- f.call("GET", "/api/debug/status", "", key, nil).Code }()
			limit := 22 * time.Second
			if scenario == "expiry" {
				limit = time.Second
			}
			select {
			case code := <-result:
				if code != 408 {
					t.Fatal(code)
				}
				if scenario == "request_timeout" && time.Since(started) < 19*time.Second {
					t.Fatal("未验证默认20秒期限")
				}
			case <-time.After(limit):
				t.Fatal("数据库等待越过绝对期限")
			}
			if len(f.s.concurrent) != 0 {
				t.Fatal("期限结束后槽未释放")
			}
		})
	}
}
