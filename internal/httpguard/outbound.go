// Package httpguard 提供 HTTP 信任边界和资源限制。
package httpguard

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

var ErrUnsafeTarget = errors.New("禁止的请求目标")
var ErrRedirectLimit = errors.New("重定向次数超限")
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

// ValidTarget 仅接受公网 HTTP(S) 的标准端口；域名还需在实际连接时验证 DNS。
func ValidTarget(u *url.URL) bool {
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
	return dnsName.MatchString(host) && !strings.Contains(host, "..") && !strings.HasPrefix(host, ".")
}

type publicTransport struct{ transport *http.Transport }

func (t *publicTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !ValidTarget(r.URL) {
		return nil, ErrUnsafeTarget
	}
	return t.transport.RoundTrip(r)
}

// NewPublicClient 不读取代理配置，将 DNS 校验和连接绑定在同一次拨号中。
// 生产调用必须传 nil；lookup/dial 仅供使用合成公网地址的隔离测试注入。
func NewPublicClient(lookup func(context.Context, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	tr := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second, MaxResponseHeaderBytes: 16 << 10,
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || (port != "80" && port != "443") {
			return nil, ErrUnsafeTarget
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
			return nil, ErrUnsafeTarget
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, ErrUnsafeTarget
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
	return &http.Client{Transport: &publicTransport{tr}, Timeout: 12 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		// 不把上一跳签名地址或认证信息传播到重定向目标。
		r.Header.Del("Referer")
		r.Header.Del("Authorization")
		r.Header.Del("Cookie")
		if len(via) > 3 {
			return ErrRedirectLimit
		}
		if !ValidTarget(r.URL) {
			return ErrUnsafeTarget
		}
		return nil
	}}
}
