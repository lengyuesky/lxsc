package httpguard

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type ioBudget struct {
	read, write, total time.Duration
	body               int64
}

func requestBudget(r *http.Request) ioBudget {
	budget := ioBudget{30 * time.Second, 60 * time.Second, 60 * time.Second, 2 << 20}
	path := strings.TrimSuffix(r.URL.Path, ".view")
	switch {
	case path == "/api/app/stream", path == "/rest/stream", path == "/rest/download":
		// 仅限制准备阶段写入，音频正文由逐次刷新的空闲截止保护，不设总时长。
		budget.total = 0
	case strings.HasPrefix(path, "/api/admin/backups/"):
		budget.read, budget.write, budget.total = 10*time.Minute, 30*time.Minute, 30*time.Minute
		if path == "/api/admin/backups/inspect" || path == "/api/admin/backups/import" {
			budget.body = (2 << 30) + (16 << 20)
		}
	case path == "/api/app/playlists/import":
		budget.read, budget.write, budget.total, budget.body = 2*time.Minute, 3*time.Minute, 3*time.Minute, 40<<20
	case strings.HasPrefix(path, "/api/admin/sources"):
		budget.read, budget.write, budget.total, budget.body = time.Minute, 90*time.Second, 90*time.Second, 16<<20
	case path == "/api/admin/metadata/compact":
		budget.write, budget.total = 5*time.Minute, 5*time.Minute
	}
	return budget
}

// LimitIO 为普通请求限制正文大小和实际套接字读写，保留专用调试接口自己的更严格期限。
func LimitIO(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/debug" || strings.HasPrefix(r.URL.Path, "/api/debug/") || r.URL.Path == "/api/admin/debug-tokens" || strings.HasPrefix(r.URL.Path, "/api/admin/debug-tokens/") {
			next.ServeHTTP(w, r)
			return
		}
		serveWithBudget(next, w, r, requestBudget(r))
	})
}

func serveWithBudget(next http.Handler, w http.ResponseWriter, r *http.Request, budget ioBudget) {
	ctx := r.Context()
	if budget.total > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget.total)
		defer cancel()
	}
	r = r.WithContext(ctx)
	r.Body = http.MaxBytesReader(w, r.Body, budget.body)
	controller := http.NewResponseController(w)
	if r.ContentLength != 0 {
		_ = controller.SetReadDeadline(time.Now().Add(budget.read))
	}
	_ = controller.SetWriteDeadline(time.Now().Add(budget.write))
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now())
		close(done)
	})
	defer func() {
		if !stop() {
			<-done
		}
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	next.ServeHTTP(w, r)
	// 成功响应保留连接生命周期；不能人为触发读超时，否则 net/http 会
	// 取消整条连接的上下文，使后续复用请求的分块响应也被截断。
	// 未读完的正文仍受原读取截止限制，完成读取后由 net/http 接管空闲等待。
	_ = controller.Flush()
}
