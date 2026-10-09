package music

import (
	"context"
	"net/http"
	"time"

	"lxsc/internal/httpguard"
)

const (
	URLCheckTimeout = 3 * time.Second
	urlCheckTTL     = 15 * time.Second
)

type urlCheckKey struct {
	key   urlKey
	token cacheToken
}

// 只保存媒体头，不保留上游请求、正文、Cookie 或重定向地址。
type mediaHeaders struct {
	status int
	header http.Header
}

func urlCheckCacheTTL(urlTTL int) time.Duration {
	if urlTTL == 0 {
		return 0
	}
	return urlCheckTTL
}

// CheckPlaybackURL 合并同一解析版本的校验，并短暂复用成功结果。
// HEAD 与随后播放共用媒体头；失败不缓存，刷新后的版本必须重新校验。
// check 必须自行关闭响应体；返回值始终是独立副本，不能修改共享缓存。
func (c *Catalog) CheckPlaybackURL(ctx context.Context, resolution URLResolution, check func(context.Context) (*http.Response, error)) (*http.Response, error) {
	result, err := c.urlChecks.load(ctx, urlCheckKey{resolution.key, resolution.token}, nil, URLCheckTimeout, func(ctx context.Context) (mediaHeaders, bool, error) {
		response, err := check(ctx)
		if response == nil {
			return mediaHeaders{}, false, err
		}
		if err == nil {
			err = httpguard.CheckMediaResponse(response)
		}
		headers := mediaHeaders{status: response.StatusCode, header: make(http.Header)}
		for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
			if value := response.Header.Get(name); value != "" {
				headers.header.Set(name, value)
			}
		}
		cacheable := err == nil && (headers.status == http.StatusOK || headers.status == http.StatusPartialContent)
		return headers, cacheable, err
	})
	if result.value.status == 0 {
		return nil, err
	}
	return &http.Response{StatusCode: result.value.status, Header: result.value.header.Clone(), Body: http.NoBody}, err
}
