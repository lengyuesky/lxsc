package coverart

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// sizedAddress 向已实测支持按需缩放的图床请求不超过 edge 的 JPEG 缩略图。
// 未知图床、无法解析或已带查询参数的地址保持原样；调用方在缩略图失败时退回原地址。
func sizedAddress(address string, edge int) string {
	if edge <= 0 || address == "" {
		return address
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return address
	}
	// 网易云专辑图常为数兆字节的 PNG 原图；图床的 imageView 等比缩小且不放大。
	if strings.HasSuffix(strings.ToLower(u.Hostname()), ".music.126.net") {
		size := strconv.Itoa(edge)
		return address + "?imageView&thumbnail=" + size + "x" + size + "&type=jpg&quality=85"
	}
	return address
}

func coverETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// notModified 按弱比较匹配 If-None-Match；代理可能把强校验值改写为弱校验值。
func notModified(r *http.Request, etag string) bool {
	if etag == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	for _, value := range r.Header.Values("If-None-Match") {
		for _, candidate := range strings.Split(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
				return true
			}
		}
	}
	return false
}
