package webauth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"lxsc/internal/db"
	"lxsc/internal/httpguard"
)

// changePassword 只使用当前会话的用户 ID，不接受账号、角色或其他用户属性。
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	u := s.Auth.User(r)
	if u == nil {
		fail(w, http.StatusUnauthorized, "未登录或会话已过期")
		return
	}
	// 仅接收同源 JSON；允许无 Origin 的非浏览器请求，但拒绝跨源及表单提交。
	origin := "http://" + r.Host
	if httpguard.Secure(r) {
		origin = "https://" + r.Host
	}
	origins := r.Header.Values("Origin")
	if site := r.Header.Get("Sec-Fetch-Site"); (site != "" && site != "same-origin") || len(origins) > 1 || (len(origins) == 1 && !strings.EqualFold(origins[0], origin)) {
		fail(w, http.StatusForbidden, "仅允许同源请求")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "请通过 JSON 正文提交密码")
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "参数错误")
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "请求体只能包含一个 JSON 对象")
		return
	}
	length := utf8.RuneCountInString(body.NewPassword)
	if body.CurrentPassword == "" || strings.TrimSpace(body.NewPassword) == "" || length < 8 || length > 128 {
		fail(w, http.StatusBadRequest, "请输入当前密码，新密码需为 8–128 个字符且不能全为空白")
		return
	}
	// 与登录共用账户限频；校验前预占，防止持有会话后并发猜测当前密码。
	if !s.Auth.allowLogin(u.Name, time.Now()) {
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "密码验证过于频繁，请一分钟后重试")
		return
	}
	plain, err := s.Auth.Secret.Decrypt(u.PasswordEnc)
	if err != nil || subtle.ConstantTimeCompare([]byte(plain), []byte(body.CurrentPassword)) != 1 {
		fail(w, http.StatusBadRequest, "当前密码错误")
		return
	}
	enc, err := s.Auth.Secret.Encrypt(body.NewPassword)
	if err != nil {
		fail(w, http.StatusInternalServerError, "密码保存失败")
		return
	}
	if err := s.Auth.DB.UpdateUserPassword(r.Context(), u.ID, u.PasswordEnc, enc); err != nil {
		if errors.Is(err, db.ErrUserPasswordChanged) {
			fail(w, http.StatusConflict, "密码已被更改，请重新登录后再试")
		} else {
			fail(w, http.StatusInternalServerError, "密码保存失败")
		}
		return
	}
	s.Auth.RevokeUser(u.ID)
	if s.PasswordChanged != nil {
		s.PasswordChanged(u.ID)
	}
	s.Auth.End(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
