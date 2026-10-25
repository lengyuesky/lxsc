package webauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func passwordRequest(handler http.Handler, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, "http://account.test/password", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://account.test")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestSelfPasswordChangeForUsersAndAdmins(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprint(admin), func(t *testing.T) {
			m, u := testManager(t)
			ctx := context.Background()
			if err := m.DB.UpdateUser(ctx, u.ID, u.Name, "", admin, "flac"); err != nil {
				t.Fatal(err)
			}
			u, _ = m.DB.GetUserByID(ctx, u.ID)
			other, err := m.DB.CreateUser(ctx, "other", u.PasswordEnc, false, "128k")
			if err != nil {
				t.Fatal(err)
			}
			first, second, otherSession := sessionRequest(m, u), sessionRequest(m, u), sessionRequest(m, other)
			cookie, _ := first.Cookie(cookieName)
			var revoked []int64
			handler := (&Server{Auth: m, PasswordChanged: func(id int64) { revoked = append(revoked, id) }}).Routes()
			w := passwordRequest(handler, cookie, `{"currentPassword":"错误密码","newPassword":"new-password"}`)
			if w.Code != 400 || m.User(first) == nil || m.User(second) == nil || len(revoked) != 0 {
				t.Fatalf("错误密码不能注销或改密: %d %s", w.Code, w.Body)
			}
			newPassword := " 新密码-测试密码 "
			body, _ := json.Marshal(map[string]string{"currentPassword": "旧密码", "newPassword": newPassword})
			w = passwordRequest(handler, cookie, string(body))
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), newPassword) {
				t.Fatalf("本人改密失败或泄露密码: %d %s", w.Code, w.Body)
			}
			if m.User(first) != nil || m.User(second) != nil || m.User(otherSession) == nil {
				t.Fatal("必须注销本人全部旧会话，但不能影响其他用户")
			}
			if len(revoked) != 1 || revoked[0] != u.ID {
				t.Fatal("只应撤销当前账号的临时凭据", revoked)
			}
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != cookieName || cookies[0].MaxAge != -1 {
				t.Fatal("必须清除当前 Cookie", cookies)
			}
			after, err := m.DB.GetUserByID(ctx, u.ID)
			if err != nil || after.Name != u.Name || after.IsAdmin != u.IsAdmin || after.Quality != u.Quality || after.PasswordEnc == newPassword || after.PasswordEnc == u.PasswordEnc {
				t.Fatalf("密码需加密保存且其他属性不变: %+v %v", after, err)
			}
			if _, err := m.Authenticate(ctx, u.Name, "旧密码"); err == nil {
				t.Fatal("旧密码不得继续登录")
			}
			if _, err := m.Authenticate(ctx, u.Name, newPassword); err != nil {
				t.Fatal("新密码应原样生效，包括首尾空格", err)
			}
			if m.User(sessionRequest(m, u)) != nil {
				t.Fatal("旧用户对象不能重新创建有效会话")
			}
		})
	}
}

func TestSelfPasswordChangeValidationAndIsolation(t *testing.T) {
	m, u := testManager(t)
	cookie, _ := sessionRequest(m, u).Cookie(cookieName)
	handler := (&Server{Auth: m}).Routes()
	for _, body := range []string{
		`{`, `null`, `{}`, `{"currentPassword":"旧密码"}`,
		`{"currentPassword":"","newPassword":"new-password"}`,
		`{"currentPassword":"旧密码","newPassword":"short"}`,
		`{"currentPassword":"旧密码","newPassword":"        "}`,
		`{"currentPassword":"旧密码","newPassword":null}`,
		`{"currentPassword":"旧密码","newPassword":"new-password","userId":2}`,
		`{"currentPassword":"旧密码","newPassword":"new-password","name":"other"}`,
		`{"currentPassword":"旧密码","newPassword":"new-password","isAdmin":true}`,
		`{"currentPassword":"旧密码","newPassword":"new-password"} {}`,
		fmt.Sprintf(`{"currentPassword":"旧密码","newPassword":%q}`, strings.Repeat("密", 129)),
		fmt.Sprintf(`{"currentPassword":%q,"newPassword":"new-password"}`, strings.Repeat("a", 17<<10)),
	} {
		if w := passwordRequest(handler, cookie, body); w.Code != 400 {
			t.Errorf("无效参数应拒绝: %.160s，状态 %d", body, w.Code)
		}
	}
	after, err := m.DB.GetUserByID(context.Background(), u.ID)
	if err != nil || *after != *u {
		t.Fatalf("校验失败不得修改用户: %+v %v", after, err)
	}
	for _, credential := range []string{"", "Bearer synthetic-token", "synthetic-api-key"} {
		r := httptest.NewRequest(http.MethodPut, "/password", strings.NewReader(`{"currentPassword":"旧密码","newPassword":"new-password"}`))
		r.Header.Set("Authorization", credential)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("仅接受有效网页登录会话", w.Code)
		}
	}
}

func TestSelfPasswordChangeRejectsCrossOriginAndSimpleForms(t *testing.T) {
	m, u := testManager(t)
	cookie, _ := sessionRequest(m, u).Cookie(cookieName)
	handler := (&Server{Auth: m}).Routes()
	for _, tc := range []struct {
		origin, site, contentType, query string
		want                             int
	}{
		{"http://evil.test", "", "application/json", "", 403},
		{"null", "", "application/json", "", 403},
		{"http://account.test", "cross-site", "application/json", "", 403},
		{"http://account.test", "same-site", "application/json", "", 403},
		{"http://account.test/", "", "application/json", "", 403},
		{"http://account.test", "", "text/plain", "", 400},
		{"http://account.test", "", "application/x-www-form-urlencoded", "", 400},
		{"http://account.test", "", "application/json", "?userId=2", 400},
	} {
		r := httptest.NewRequest(http.MethodPut, "http://account.test/password"+tc.query, strings.NewReader(`{"currentPassword":"旧密码","newPassword":"new-password"}`))
		r.AddCookie(cookie)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		r.Header.Set("Content-Type", tc.contentType)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%+v: %d %s", tc, w.Code, w.Body)
		}
	}
}

func TestSelfPasswordChangeRateLimit(t *testing.T) {
	m, u := testManager(t)
	cookie, _ := sessionRequest(m, u).Cookie(cookieName)
	handler := (&Server{Auth: m}).Routes()
	for range 10 {
		if w := passwordRequest(handler, cookie, `{"currentPassword":"wrong","newPassword":"new-password"}`); w.Code != 400 {
			t.Fatal("限额内错误密码应返回400", w.Code)
		}
	}
	w := passwordRequest(handler, cookie, `{"currentPassword":"旧密码","newPassword":"new-password"}`)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
		t.Fatal("过多尝试必须限频", w.Code, w.Header())
	}
	if _, err := m.Authenticate(context.Background(), u.Name, "旧密码"); err != nil {
		t.Fatal("限频不得修改数据库密码", err)
	}
}
