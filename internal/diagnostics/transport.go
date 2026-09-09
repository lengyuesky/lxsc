package diagnostics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var errUnsafeTarget = errors.New("禁止的探测目标")
var errRedirectLimit = errors.New("重定向次数超限")
var dnsName = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
var deniedPrefixes = func() []netip.Prefix {
	// 包含私网、元数据、CGNAT、文档、基准、保留及 IPv6 转换/隧道地址。
	values := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.175.48.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "2001::/23", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20"}
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		out = append(out, netip.MustParsePrefix(v))
	}
	return out
}()

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Is4In6() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range deniedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
func validTarget(u *url.URL) bool {
	if u == nil || len(u.String()) > 4096 || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if strings.HasSuffix(u.Host, ":") || (u.Port() != "" && u.Port() != "80" && u.Port() != "443") {
		return false
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		return publicIP(ip)
	}
	return dnsName.MatchString(host) && !strings.Contains(host, "..") && !strings.HasPrefix(host, ".") && !strings.HasSuffix(u.Host, ":")
}

type safeTransport struct{ transport *http.Transport }

func (t *safeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !validTarget(r.URL) {
		return nil, errUnsafeTarget
	}
	return t.transport.RoundTrip(r)
}

// newProbeClient 不读取环境或配置代理；DNS 校验与实际连接在同一 DialContext 中完成。
// lookup/dial 参数仅用于隔离测试，生产固定使用系统解析器与普通直连。
func newProbeClient(lookup func(context.Context, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	tr := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second, MaxResponseHeaderBytes: 16 << 10,
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || (port != "80" && port != "443") {
			return nil, errUnsafeTarget
		}
		var ips []netip.Addr
		if ip, parseErr := netip.ParseAddr(host); parseErr == nil {
			ips = []netip.Addr{ip}
		} else {
			ips, err = lookup(ctx, host)
			if err != nil {
				return nil, err
			}
		}
		if len(ips) == 0 || len(ips) > 32 {
			return nil, errUnsafeTarget
		}
		// 混合公私网解析也全部拒绝，不能先连接再检查。
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errUnsafeTarget
			}
		}
		var last error
		for _, ip := range ips {
			conn, err := dial(ctx, "tcp", net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, last
	}
	return &http.Client{Transport: &safeTransport{tr}, Timeout: 12 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		// net/http 默认会把上一跳的签名路径/查询放入 Referer；探测不能转发这些值。
		r.Header.Del("Referer")
		if len(via) > 3 {
			return errRedirectLimit
		}
		if !validTarget(r.URL) {
			return errUnsafeTarget
		}
		return nil
	}}
}
func probeClient() *http.Client {
	return newProbeClient(func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}, (&net.Dialer{Timeout: 5 * time.Second}).DialContext)
}
