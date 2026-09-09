// Package diagnostics 提供与原始日志隔离的限权、限时播放诊断。
package diagnostics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"regexp"
	"sync"
	"syscall"
	"time"
)

// Event 只接受固定字段；不得加入 URL、用户名、歌曲文本或自由文本错误。
type Event struct {
	Time      time.Time `json:"time"`
	Stage     string    `json:"stage"`
	TrackID   string    `json:"trackId,omitempty"`
	Platform  string    `json:"platform,omitempty"`
	Quality   string    `json:"quality,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Cached    bool      `json:"cached"`
	Status    int       `json:"status"`
	Error     string    `json:"error"`
	ElapsedMS int64     `json:"elapsedMs"`
}

var trackPattern = regexp.MustCompile(`^tr-(wy|tx|kw|kg|mg)-[A-Za-z0-9_-]{1,128}$`)

func ValidTrackID(id string) bool { return trackPattern.MatchString(id) }
func oneOf(value string, values ...string) string {
	for _, v := range values {
		if v == value {
			return value
		}
	}
	return ""
}
func Platform(value string) string { return oneOf(value, "wy", "tx", "kw", "kg", "mg") }
func Quality(value string) string {
	return oneOf(value, "128k", "192k", "320k", "flac", "flac24bit", "hires", "atmos", "atmos_plus", "master", "dolby")
}
func Mode(value string) string { return oneOf(value, "redirect", "force_redirect", "proxy") }

// ErrorCode 通过类型分类；绝不读取或返回 err.Error() 中的签名地址。
func ErrorCode(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, errUnsafeTarget) {
		return "blocked_target"
	}
	if errors.Is(err, errRedirectLimit) {
		return "redirect_limit"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.Timeout() {
			return "timeout"
		}
		return "dns"
	}
	var cert *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	if errors.As(err, &cert) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &record) {
		return "tls"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "connect"
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) {
		return "connect"
	}
	return "upstream_error"
}

// Events 固定容量内存环；零值可用，nil 接收者方便不启用诊断的测试服务。
type Events struct {
	mu      sync.Mutex
	entries []Event
}

func (b *Events) Add(e Event) {
	if b == nil {
		return
	}
	e.Stage = oneOf(e.Stage, "metadata", "resolve", "refresh", "cache_check", "redirect", "proxy_response", "proxy_copy", "probe")
	if e.Stage == "" {
		return
	}
	if !ValidTrackID(e.TrackID) {
		e.TrackID = ""
	}
	e.Platform = Platform(e.Platform)
	e.Quality = Quality(e.Quality)
	e.Mode = Mode(e.Mode)
	e.Error = oneOf(e.Error, "none", "cancelled", "timeout", "dns", "tls", "connect", "blocked_target", "redirect_limit", "upstream_error")
	if e.Error == "" {
		e.Error = "upstream_error"
	}
	if e.Status < 100 || e.Status > 599 {
		e.Status = 0
	}
	if e.ElapsedMS < 0 {
		e.ElapsedMS = 0
	}
	if e.ElapsedMS > 86400000 {
		e.ElapsedMS = 86400000
	}
	e.Time = time.Now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 200 {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:199]
	}
	b.entries = append(b.entries, e)
}
func (b *Events) List() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, len(b.entries))
	copy(out, b.entries)
	return out
}
