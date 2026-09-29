package music

import (
	"context"
	"time"

	"lxsc/internal/admission"
)

// AcquireRequest 的额度由同一个 Catalog 跨网页、管理端和 Subsonic 共享。
// 媒体请求只在准备响应头时占用额度，传输正文之前释放。
func (c *Catalog) AcquireRequest(ctx context.Context, userID int64) (func(), error) {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	release, err := c.RequestLimits.Acquire(waitCtx, userID)
	if err != nil && ctx.Err() == nil {
		return nil, admission.ErrBusy
	}
	return release, err
}
