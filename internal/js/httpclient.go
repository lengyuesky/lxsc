package js

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// NewHTTPClients 构建上游请求客户端（校验证书 / 不校验证书两套），支持 http(s)/socks5 代理
func NewHTTPClients(proxyURL string) (secure, insecure *http.Client, err error) {
	mk := func(skipVerify bool) (*http.Client, error) {
		tr := &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          200,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: skipVerify}, //nolint:gosec // 仅用于显式要求跳过校验的请求
		}
		if proxyURL != "" {
			u, err := url.Parse(proxyURL)
			if err != nil {
				return nil, fmt.Errorf("代理地址无效: %w", err)
			}
			switch u.Scheme {
			case "http", "https":
				tr.Proxy = http.ProxyURL(u)
			case "socks5", "socks5h", "socks":
				d, err := proxy.FromURL(u, proxy.Direct)
				if err != nil {
					return nil, fmt.Errorf("socks 代理无效: %w", err)
				}
				if cd, ok := d.(proxy.ContextDialer); ok {
					tr.DialContext = cd.DialContext
				} else {
					tr.Dial = d.Dial //nolint:staticcheck
				}
			default:
				return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
			}
		}
		return &http.Client{Transport: tr, Timeout: 0}, nil
	}
	secure, err = mk(false)
	if err != nil {
		return nil, nil, err
	}
	insecure, err = mk(true)
	if err != nil {
		return nil, nil, err
	}
	return secure, insecure, nil
}
