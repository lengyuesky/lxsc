package diagnostics

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"

	"lxsc/internal/httpguard"
)

// 与封面代理共用目标校验，保留诊断错误分类和隔离测试入口。
var errUnsafeTarget = httpguard.ErrUnsafeTarget
var errRedirectLimit = httpguard.ErrRedirectLimit

func validTarget(u *url.URL) bool { return httpguard.ValidTarget(u) }

func newProbeClient(lookup func(context.Context, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	return httpguard.NewPublicClient(lookup, dial)
}

func probeClient() *http.Client { return httpguard.NewPublicClient(nil, nil) }
