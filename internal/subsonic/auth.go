package subsonic

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"lxsc/internal/db"
)

type ctxKey int

const userKey ctxKey = 1

// currentUser 取当前请求用户
func currentUser(r *http.Request) *db.User {
	u, _ := r.Context().Value(userKey).(*db.User)
	return u
}

// authenticate 支持 u+p（明文/enc:hex）、u+t+s（md5(password+salt)）、apiKey
func (s *Server) authenticate(r *http.Request) (*db.User, int, string) {
	ctx := r.Context()
	if key := param(r, "apiKey"); key != "" {
		if param(r, "u") != "" || param(r, "p") != "" || param(r, "t") != "" {
			return nil, 43, "Multiple conflicting authentication mechanisms provided"
		}
		u, err := s.DB.GetUserByAPIKey(ctx, key)
		if err != nil {
			return nil, 44, "Invalid API key"
		}
		return u, 0, ""
	}
	name := param(r, "u")
	if name == "" {
		return nil, ErrMissingParam, "Required parameter is missing: u"
	}
	u, err := s.DB.GetUserByName(ctx, name)
	if err != nil {
		return nil, ErrWrongAuth, "Wrong username or password"
	}
	pw, err := s.Secret.Decrypt(u.PasswordEnc)
	if err != nil {
		return nil, ErrGeneric, "server error"
	}
	if t, salt := param(r, "t"), param(r, "s"); t != "" && salt != "" {
		sum := md5.Sum([]byte(pw + salt))
		if !strings.EqualFold(hex.EncodeToString(sum[:]), t) {
			return nil, ErrWrongAuth, "Wrong username or password"
		}
		return u, 0, ""
	}
	p := param(r, "p")
	if p == "" {
		return nil, ErrMissingParam, "Required parameter is missing: p or t/s"
	}
	if strings.HasPrefix(p, "enc:") {
		b, err := hex.DecodeString(p[4:])
		if err != nil {
			return nil, ErrWrongAuth, "Wrong username or password"
		}
		p = string(b)
	}
	if p != pw {
		return nil, ErrWrongAuth, "Wrong username or password"
	}
	return u, 0, ""
}

// withUser 放入上下文
func withUser(ctx context.Context, u *db.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

var errUnauthorized = errors.New("unauthorized")
