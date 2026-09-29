package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// compactMetadata 必须在维护窗口显式调用，不再隐含在普通元数据清理中。
func (s *Server) compactMetadata(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || !body.Confirm {
		fail(w, http.StatusBadRequest, "请确认压缩期间业务写入可能暂停")
		return
	}
	if !s.maintenance.CompareAndSwap(false, true) {
		fail(w, http.StatusConflict, "正在执行数据库维护")
		return
	}
	defer s.maintenance.Store(false)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := s.DB.Compact(ctx); err != nil {
		fail(w, http.StatusInternalServerError, "数据库压缩未完成，请在低峰期重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
