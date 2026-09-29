package webauth

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lxsc/internal/db"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
)

func testManager(t *testing.T) (*Manager, *db.User) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	box, err := secret.Load(dir, "测试密钥")
	if err != nil {
		t.Fatal(err)
	}
	password, err := box.Encrypt("旧密码")
	if err != nil {
		t.Fatal(err)
	}
	u, err := database.CreateUser(context.Background(), "用户", password, false, "320k")
	if err != nil {
		t.Fatal(err)
	}
	return &Manager{DB: database, Secret: box}, u
}

func sessionRequest(m *Manager, u *db.User) *http.Request {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	m.Start(w, r, u)
	r.AddCookie(w.Result().Cookies()[0])
	return r
}

func TestSessionPasswordChangeAndStaleLogin(t *testing.T) {
	m, user := testManager(t)
	old := sessionRequest(m, user)
	if m.User(old) == nil {
		t.Fatal("新会话应可使用")
	}
	enc, _ := m.Secret.Encrypt("新密码")
	if err := m.DB.UpdateUser(context.Background(), user.ID, user.Name, enc, false, "320k"); err != nil {
		t.Fatal(err)
	}
	if m.User(old) != nil {
		t.Fatal("改密必须使已有会话失效")
	}
	if m.User(sessionRequest(m, user)) != nil {
		t.Fatal("改密前读取的用户对象不能在改密后重新创建有效会话")
	}
	fresh, err := m.Authenticate(context.Background(), user.Name, "新密码")
	if err != nil {
		t.Fatal(err)
	}
	r := sessionRequest(m, fresh)
	if m.User(r) == nil {
		t.Fatal("新密码登录应成功")
	}
	m.RevokeUser(user.ID)
	if m.User(r) != nil {
		t.Fatal("撤销必须立即生效")
	}
}

func TestSessionPruningAndCapacity(t *testing.T) {
	m, user := testManager(t)
	first := sessionRequest(m, user)
	for range 10 {
		sessionRequest(m, user)
	}
	if m.User(first) != nil {
		t.Fatal("第十一个会话必须淘汰最早会话")
	}
	count := 0
	m.sessions.Range(func(_, _ any) bool { count++; return true })
	if count != 10 {
		t.Fatalf("单用户应保留十个会话: %d", count)
	}
	for i := 0; i < 4096; i++ {
		m.sessions.Store(fmt.Sprint(i), session{userID: 100, expires: time.Now().Add(time.Hour)})
	}
	sessionRequest(m, user)
	count = 0
	m.sessions.Range(func(_, _ any) bool { count++; return true })
	if count != 4096 {
		t.Fatalf("全局会话上限失效: %d", count)
	}
	m.mu.Lock()
	m.pruneLocked(time.Now().Add(8 * 24 * time.Hour))
	m.mu.Unlock()
	count = 0
	m.sessions.Range(func(_, _ any) bool { count++; return true })
	if count != 0 {
		t.Fatal("过期会话必须被主动清理")
	}
}

func TestLoginLimitConcurrentAndExpiry(t *testing.T) {
	m := &Manager{}
	at := time.Now()
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 40 {
		wg.Go(func() {
			if m.allowLogin("用户", at) {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if allowed.Load() != 10 {
		t.Fatalf("并发尝试必须共享限额: %d", allowed.Load())
	}
	if !m.allowLogin("其他用户", at) {
		t.Fatal("另一用户应有独立限额")
	}
	if !m.allowLogin("用户", at.Add(time.Minute)) {
		t.Fatal("窗口结束应恢复尝试")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Maintain(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("清理协程未退出")
	}
}

func TestLoginHTTPRateLimitAndSuccessReset(t *testing.T) {
	m, _ := testManager(t)
	store, err := settings.New(context.Background(), m.DB)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Auth: m, Settings: store}
	login := func(password string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/login", bytes.NewBufferString(fmt.Sprintf(`{"username":" 用户 ","password":%q}`, password)))
		s.Routes().ServeHTTP(w, r)
		return w
	}
	for range 9 {
		if login("错误密码").Code != 401 {
			t.Fatal("错误密码应返回 401")
		}
	}
	if login("旧密码").Code != 200 {
		t.Fatal("限额内正确登录应成功")
	}
	for range 10 {
		if login("错误密码").Code != 401 {
			t.Fatal("成功登录应重置失败计数")
		}
	}
	limited := login("旧密码")
	if limited.Code != 429 || limited.Header().Get("Retry-After") != "60" {
		t.Fatal("超限应返回 429 与重试时间")
	}
}
