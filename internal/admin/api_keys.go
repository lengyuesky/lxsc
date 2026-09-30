package admin

import (
	"encoding/json"
	"errors"
	"github.com/go-chi/chi/v5"
	"io"
	"lxsc/internal/secret"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "用户 ID 无效")
		return
	}
	var body struct {
		Label string `json:"label"`
		Days  int    `json:"days"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &body); err != nil {
			fail(w, 400, "参数错误")
			return
		}
	}
	body.Label = strings.TrimSpace(body.Label)
	if utf8.RuneCountInString(body.Label) > 100 || body.Days < 0 || body.Days > 3650 {
		fail(w, 400, "名称最多100字，有效期为0–3650天，0表示长期有效")
		return
	}
	if body.Label == "" {
		body.Label = "客户端"
	}
	var expires int64
	if body.Days > 0 {
		expires = time.Now().Add(time.Duration(body.Days) * 24 * time.Hour).Unix()
	}
	key := "lxsc_" + secret.RandomToken(24)
	if _, err = s.DB.GetUserByID(r.Context(), id); err != nil {
		fail(w, 404, "用户不存在")
		return
	}
	if err = s.DB.CreateExpiringAPIKey(r.Context(), id, key, body.Label, expires); err != nil {
		fail(w, 500, "创建密钥失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"apiKey": key, "expiresAt": expires})
}
func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "用户 ID 无效")
		return
	}
	keys, err := s.DB.ListAPIKeys(r.Context(), id)
	if err != nil {
		fail(w, 500, "读取密钥失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, keys)
}
func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "用户 ID 无效")
		return
	}
	if err = s.DB.RevokeAPIKey(r.Context(), id, chi.URLParam(r, "keyID")); err != nil {
		fail(w, 404, "密钥不存在或撤销失败")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("请求体只能包含一个JSON对象")
	}
	return nil
}
