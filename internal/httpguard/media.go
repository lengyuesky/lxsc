package httpguard

import (
	"errors"
	"net/http"
	"strings"
)

var ErrNonAudioResponse = errors.New("音频地址返回了非音频内容")

// CheckMediaResponse 只检查响应头，不读取正文。缺少类型或通用二进制类型仍可播放；
// 明确的文本、图片、JSON/XML 错误页不能仅凭 200/206 被当作音频。
func CheckMediaResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil
	}
	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(contentType, "text/") || strings.HasPrefix(contentType, "image/") ||
		contentType == "application/json" || contentType == "application/xml" ||
		strings.HasSuffix(contentType, "+json") || strings.HasSuffix(contentType, "+xml") {
		return ErrNonAudioResponse
	}
	return nil
}
