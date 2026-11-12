package portal

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"lxsc/internal/db"
)

func (s *Server) recentPlayback(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.listeningScope(w, r)
	if !ok {
		return
	}
	params := r.URL.Query()
	q := db.PlaybackQuery{UserID: userID, Query: strings.TrimSpace(params.Get("query")), Client: params.Get("client"), Delivery: params.Get("delivery"), Outcome: params.Get("outcome"), Cursor: params.Get("cursor"), Days: 30, Limit: 50}
	if utf8.RuneCountInString(q.Query) > 200 || utf8.RuneCountInString(q.Client) > 64 || !slices.Contains([]string{"", "302", "proxy"}, q.Delivery) || !slices.Contains([]string{"", "pending", "success", "failed", "interrupted", "reported", "unknown"}, q.Outcome) {
		fail(w, 400, "日志筛选条件无效")
		return
	}
	for _, field := range []struct {
		name   string
		target *int
	}{{"days", &q.Days}, {"limit", &q.Limit}} {
		if raw := params.Get(field.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				fail(w, 400, "日志查询范围无效")
				return
			}
			*field.target = value
		}
	}
	if !slices.Contains([]int{7, 30, 90, 365}, q.Days) || q.Limit < 1 || q.Limit > 100 {
		fail(w, 400, "日志查询范围无效")
		return
	}
	if raw := params.Get("sourceId"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value <= 0 {
			fail(w, 400, "音源编号无效")
			return
		}
		q.SourceID = value
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	page, err := s.DB.RecentPlayback(ctx, q, time.Now())
	if err != nil {
		if errors.Is(err, db.ErrPlaybackLog) {
			fail(w, 400, "日志查询条件无效")
		} else {
			fail(w, 503, "读取播放日志失败，请稍后重试")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, page)
}

func (s *Server) recentPlaybackDetail(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.listeningScope(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "日志编号无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	detail, err := s.DB.PlaybackDetail(ctx, id, userID, time.Now())
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			fail(w, 404, "日志不存在或已过期")
		} else {
			fail(w, 503, "读取播放详情失败，请稍后重试")
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, detail)
}
