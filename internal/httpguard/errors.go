package httpguard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"syscall"
)

// ErrorCode 通过类型分类；绝不读取或返回 err.Error() 中的签名地址。
func ErrorCode(err error) string {
	if err == nil {
		return "none"
	}
	var classified interface{ DiagnosticCode() string }
	if errors.As(err, &classified) {
		switch code := classified.DiagnosticCode(); code {
		case "expired", "rate_limited", "upstream_status", "unsupported_quality", "source_auth", "invalid_response", "source_error", "dns", "tls", "timeout", "connect", "cancelled", "blocked_target", "redirect_limit", "non_audio":
			return code
		}
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, ErrUnsafeTarget) {
		return "blocked_target"
	}
	if errors.Is(err, ErrRedirectLimit) {
		return "redirect_limit"
	}
	if errors.Is(err, ErrNonAudioResponse) {
		return "non_audio"
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

// SanitizedError 只缓存固定错误类别，不保留网络错误中的地址或证书信息。
func SanitizedError(err error) error {
	if err == nil {
		return nil
	}
	return sanitizedError(ErrorCode(err))
}

type sanitizedError string

func (e sanitizedError) Error() string          { return string(e) }
func (e sanitizedError) DiagnosticCode() string { return string(e) }
func (e sanitizedError) Is(target error) bool {
	return (e == "timeout" && target == context.DeadlineExceeded) || (e == "cancelled" && target == context.Canceled) || (e == "non_audio" && target == ErrNonAudioResponse)
}
