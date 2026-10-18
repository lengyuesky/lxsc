package music

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"lxsc/internal/httpguard"
)

const (
	URLCheckTimeout    = 3 * time.Second
	urlCheckTTL        = 15 * time.Second
	urlCheckFailureTTL = 3 * time.Second
)

type urlCheckKey struct {
	token cacheToken
}

// 只保存媒体头，不保留上游请求、正文、Cookie 或重定向地址。
type mediaHeaders struct {
	status  int
	header  http.Header
	failure error // 固定类别，不保留原始网络错误中的地址
}

var errMediaHeaderUnavailable = errors.New("媒体响应头暂不可用")

func playbackCheckExpiry(_ urlCheckKey, value mediaHeaders, created, expires time.Time) time.Time {
	if value.failure != nil || value.status >= 400 {
		limit := created.Add(urlCheckFailureTTL)
		if expires.IsZero() || limit.Before(expires) {
			return limit
		}
	}
	return expires
}

func urlCheckCacheTTL(urlTTL int) time.Duration {
	if urlTTL == 0 {
		return 0
	}
	return urlCheckTTL
}

// CheckPlaybackURL 合并同一解析版本的校验，并短暂复用成功结果。
// HEAD 与随后播放共用媒体头；失败短暂保存为失败，新解析版本必须重新校验。
// check 必须自行关闭响应体；返回值始终是独立副本，不能修改共享缓存。
func (c *Catalog) CheckPlaybackURL(ctx context.Context, resolution URLResolution, check func(context.Context) (*http.Response, error)) (*http.Response, error) {
	result, err := c.urlChecks.load(ctx, urlCheckKey{resolution.token}, nil, URLCheckTimeout, func(ctx context.Context) (mediaHeaders, bool, error) {
		checkCtx, cancel := context.WithTimeout(ctx, URLCheckTimeout-250*time.Millisecond)
		response, err := check(checkCtx)
		cancel()
		if errors.Is(err, context.Canceled) {
			return mediaHeaders{}, false, err
		}
		headers := mediaHeaders{header: make(http.Header)}
		if response == nil && err == nil {
			return headers, false, errMediaHeaderUnavailable
		}
		if err == nil {
			err = httpguard.CheckMediaResponse(response)
		}
		headers.failure = httpguard.SanitizedError(err)
		if response == nil {
			return headers, true, nil
		}
		headers.status = response.StatusCode
		for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
			if value := response.Header.Get(name); value != "" {
				headers.header.Set(name, value)
			}
		}
		if seconds, parseErr := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64); parseErr == nil && seconds > 0 {
			headers.header.Set("Retry-After", strconv.FormatInt(min(seconds, 300), 10))
		} else if date, parseErr := http.ParseTime(response.Header.Get("Retry-After")); parseErr == nil && time.Until(date) > 0 {
			headers.header.Set("Retry-After", strconv.FormatInt(min(300, int64(time.Until(date)/time.Second)+1), 10))
		}
		return headers, true, nil
	})
	if err == nil {
		err = result.value.failure
	}
	if result.value.status == 0 {
		return nil, err
	}
	return &http.Response{StatusCode: result.value.status, Header: result.value.header.Clone(), Body: http.NoBody}, err
}
