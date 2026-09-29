package httpguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type secureKey struct{}

// TrustedPrefixes 拒绝误写的网段；不隐式信任所有内网或回环连接。
func TrustedPrefixes(values []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("可信代理网段无效: %q", value)
		}
		out = append(out, prefix.Masked())
	}
	return out, nil
}

// Secure 仅接受真实 TLS 或已经由可信代理中间件验证的 HTTPS 标记。
func Secure(r *http.Request) bool {
	secure, _ := r.Context().Value(secureKey{}).(bool)
	return r.TLS != nil || secure
}

// ProxyHeaders 从连接对端开始向左剥离可信代理，不能信任客户端自填的首个 XFF。
// 代理必须覆盖 X-Forwarded-Proto，并正确追加或重写 X-Forwarded-For。
func ProxyHeaders(prefixes []netip.Prefix) func(http.Handler) http.Handler {
	trusted := func(ip netip.Addr) bool {
		for _, prefix := range prefixes {
			if prefix.Contains(ip.Unmap()) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			peer, err := netip.ParseAddr(host)
			if err == nil && trusted(peer) {
				if values := r.Header.Values("X-Forwarded-Proto"); len(values) == 1 && values[0] == "https" {
					r = r.WithContext(context.WithValue(r.Context(), secureKey{}, true))
				}
				chain := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
				if len(chain) <= 32 {
					client, valid := peer, true
					for i := len(chain) - 1; i >= 0 && trusted(client); i-- {
						ip, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
						if err != nil || ip.Zone() != "" {
							valid = false
							break
						}
						client = ip.Unmap()
					}
					if valid {
						r.RemoteAddr = client.String()
					}
				}
			}
			// 下游不应绕过上述校验再次解析原始转发头；Host 保持原样供同源校验。
			for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Real-IP"} {
				r.Header.Del(name)
			}
			next.ServeHTTP(w, r)
		})
	}
}
