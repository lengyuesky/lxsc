// Package chksz 调用 ChKSz 公共音乐 API（https://api.chksz.com），
// 在全部音源都失败后为网易云、QQ 音乐和酷狗曲目取播放地址。
//
// 客户端只负责一次取链：拼请求、解析响应、按密钥失效或限流暂停调用。
// 缓存、选曲与音频验证由调用方负责。错误信息不包含请求地址，避免密钥进入日志或诊断。
package chksz

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL 是 ChKSz API 的公开地址。
const DefaultBaseURL = "https://api.chksz.com"

// SourceName 在播放日志与诊断中标识兜底接口给出的地址。
const SourceName = "ChKSz API 兜底"

const (
	maxResponseBytes = 1 << 20
	// authBlock 是密钥被拒后暂停调用的时间；更换密钥立即恢复。
	authBlock = 10 * time.Minute
	// limitBlock 是限流且未给出 Retry-After 时暂停调用的时间。
	limitBlock = 5 * time.Minute
	maxBlock   = time.Hour
)

var (
	ErrNotConfigured = errors.New("未配置 ChKSz API 密钥")
	ErrUnsupported   = errors.New("ChKSz API 不支持该平台")
	ErrAuth          = errors.New("ChKSz API 密钥无效或已失效")
	ErrRateLimited   = errors.New("ChKSz API 额度用尽或请求受限")
	ErrUnavailable   = errors.New("ChKSz API 暂不可用")
	ErrNoURL         = errors.New("ChKSz API 未返回可用播放地址")
	ErrInvalid       = errors.New("ChKSz API 返回无法识别的响应")
	// ErrKeyInURL 表示返回的播放地址携带了密钥：302 交付会把密钥交给客户端，因此拒绝使用。
	ErrKeyInURL = errors.New("ChKSz API 返回的播放地址包含密钥")
)

// Error 只保留受控类别，供诊断映射为固定错误码。
type Error struct{ cause error }

func (e *Error) Error() string { return e.cause.Error() }
func (e *Error) Unwrap() error { return e.cause }

// DiagnosticCode 与音源错误使用相同的错误码，播放日志无需另设分类。
func (e *Error) DiagnosticCode() string {
	switch e.cause {
	case ErrAuth:
		return "source_auth"
	case ErrRateLimited:
		return "rate_limited"
	case ErrNoURL:
		return "unsupported_quality"
	case ErrInvalid, ErrKeyInURL:
		return "invalid_response"
	default:
		return "source_error"
	}
}

func fail(cause error) error { return &Error{cause: cause} }

// Result 是一次取链的结果，Quality 为内部档位（128k/320k/flac/flac24bit/master）。
type Result struct {
	URL     string
	Quality string
}

// Client 可并发使用。零值不可用，请使用 New。
type Client struct {
	HTTP    *http.Client
	BaseURL string

	now          func() time.Time
	mu           sync.Mutex
	blockedKey   string
	blockedUntil time.Time
	blockedErr   error
}

func New(client *http.Client) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{HTTP: client, BaseURL: DefaultBaseURL, now: time.Now}
}

// Supports 报告平台是否有对应接口：网易云 wy、QQ 音乐 tx、酷狗 kg。
func Supports(platform string) bool {
	return platform == "wy" || platform == "tx" || platform == "kg"
}

// Blocked 报告该密钥此刻是否因认证失败或限流暂停调用。
func (c *Client) Blocked(key string) bool {
	return c.blocked(key) != nil
}

func (c *Client) blocked(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockedKey == key && c.now().Before(c.blockedUntil) {
		return c.blockedErr
	}
	return nil
}

func (c *Client) block(key string, err error, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until := c.now().Add(min(maxBlock, d))
	if c.blockedKey != key || until.After(c.blockedUntil) {
		c.blockedKey, c.blockedUntil, c.blockedErr = key, until, err
	}
}

// Resolve 向 ChKSz 请求一首歌在指定音质下的播放地址。
// id 为平台歌曲 ID：网易云数字 ID、QQ 音乐 songmid、酷狗 hash。
func (c *Client) Resolve(ctx context.Context, key, platform, id, quality string) (Result, error) {
	key, id = strings.TrimSpace(key), strings.TrimSpace(id)
	if key == "" {
		return Result{}, fail(ErrNotConfigured)
	}
	if !Supports(platform) || id == "" {
		return Result{}, fail(ErrUnsupported)
	}
	if err := c.blocked(key); err != nil {
		return Result{}, err
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	query := url.Values{"type": {"json"}, "apikey": {key}}
	var path string
	switch platform {
	case "wy":
		path = "/api/163_music"
		query.Set("id", id)
		query.Set("level", neteaseLevel(quality))
	case "tx":
		path = "/api/qq_music"
		query.Set("mid", id)
		query.Set("size", nativeSize(quality))
	case "kg":
		path = "/api/kugou_music"
		query.Set("id", id)
		query.Set("size", nativeSize(quality))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path+"?"+query.Encode(), nil)
	if err != nil {
		return Result{}, fail(ErrUnavailable)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		// url.Error 会带上完整请求地址（含密钥），只保留内层网络错误。
		var wrapped *url.Error
		if errors.As(err, &wrapped) && wrapped.Err != nil {
			return Result{}, wrapped.Err
		}
		return Result{}, fail(ErrUnavailable)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fail(ErrUnavailable)
	}
	if len(body) > maxResponseBytes {
		return Result{}, fail(ErrInvalid)
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	code := resp.StatusCode
	if code == http.StatusOK {
		if value, ok := jsonInt(envelope.Code); ok {
			code = value
		}
	}
	if code != http.StatusOK {
		return Result{}, c.statusError(key, code, resp.Header.Get("Retry-After"))
	}
	result, err := parse(platform, quality, body)
	if err != nil {
		return Result{}, err
	}
	if leaksKey(result.URL, key) {
		return Result{}, fail(ErrKeyInURL)
	}
	return result, nil
}

// leaksKey 判断播放地址是否携带密钥。过短的密钥做子串比较会误伤普通地址，只比对查询参数值。
func leaksKey(address, key string) bool {
	if len(key) >= 8 && (strings.Contains(address, key) || strings.Contains(address, url.QueryEscape(key))) {
		return true
	}
	if parsed, err := url.Parse(address); err == nil {
		for _, values := range parsed.Query() {
			for _, value := range values {
				if value == key {
					return true
				}
			}
		}
	}
	return false
}

func (c *Client) statusError(key string, code int, retryAfter string) error {
	switch {
	case code == http.StatusUnauthorized:
		err := fail(ErrAuth)
		c.block(key, err, authBlock)
		return err
	case code == http.StatusForbidden || code == http.StatusTooManyRequests:
		wait := limitBlock
		if seconds, err := strconv.ParseInt(strings.TrimSpace(retryAfter), 10, 64); err == nil && seconds > 0 {
			wait = time.Duration(min(seconds, int64(maxBlock/time.Second))) * time.Second
		} else if date, err := http.ParseTime(retryAfter); err == nil && date.After(c.now()) {
			wait = date.Sub(c.now())
		}
		err := fail(ErrRateLimited)
		c.block(key, err, wait)
		return err
	case code == http.StatusBadRequest || code == http.StatusNotFound:
		// 歌曲不存在、无版权或该音质不可用：只说明这首歌在这一档取不到。
		return fail(ErrNoURL)
	default:
		return fail(ErrUnavailable)
	}
}

func parse(platform, requested string, body []byte) (Result, error) {
	var fields struct {
		URL     string `json:"url"`
		Bitrate string `json:"bitrate"`
		Level   string `json:"level"`
		Data    *struct {
			URL   string `json:"url"`
			Level string `json:"level"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return Result{}, fail(ErrInvalid)
	}
	address, actual := fields.URL, ""
	if platform == "wy" {
		if fields.Data != nil {
			address, actual = fields.Data.URL, levelQuality(fields.Data.Level)
		}
	} else {
		actual = sizeQuality(fields.Bitrate)
	}
	address = strings.TrimSpace(address)
	if address == "" {
		return Result{}, fail(ErrNoURL)
	}
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return Result{}, fail(ErrInvalid)
	}
	if actual == "" {
		// 未声明实际档位时按实际请求的档位记录，例如 192k 请求的是 128k。
		if platform == "wy" {
			actual = levelQuality(neteaseLevel(requested))
		} else {
			actual = sizeQuality(nativeSize(requested))
		}
	}
	return Result{URL: address, Quality: actual}, nil
}

func jsonInt(raw json.RawMessage) (int, bool) {
	var number int
	if json.Unmarshal(raw, &number) == nil {
		return number, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if value, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
			return value, true
		}
	}
	return 0, false
}

// rank 与 js.QualityRank 一致，未知档位按 320k 处理；本包不依赖脚本运行时。
func rank(quality string) int {
	switch quality {
	case "128k":
		return 1
	case "192k":
		return 2
	case "320k":
		return 3
	case "flac":
		return 4
	case "flac24bit", "hires":
		return 5
	case "master", "atmos", "atmos_plus", "dolby":
		return 6
	}
	return 3
}

// neteaseLevel 把内部档位映射为网易云 level，不高于请求档位。
func neteaseLevel(quality string) string {
	return [...]string{"standard", "standard", "exhigh", "lossless", "hires", "jymaster"}[rank(quality)-1]
}

// nativeSize 把内部档位映射为 QQ 音乐与酷狗接口的 size，不高于请求档位。
func nativeSize(quality string) string {
	return [...]string{"128k", "128k", "320k", "flac", "hires", "master"}[rank(quality)-1]
}

func levelQuality(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "standard":
		return "128k"
	case "higher":
		return "192k"
	case "exhigh":
		return "320k"
	case "lossless":
		return "flac"
	case "hires":
		return "flac24bit"
	case "jymaster":
		return "master"
	}
	return ""
}

func sizeQuality(size string) string {
	switch value := strings.ToLower(strings.TrimSpace(size)); value {
	case "128k", "320k", "flac", "master":
		return value
	case "hires":
		return "flac24bit"
	}
	return ""
}
