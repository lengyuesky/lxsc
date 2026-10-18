package js

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSourceAuth        = errors.New("音源认证失效")
	ErrSourceRateLimited = errors.New("音源暂时限流")
	ErrSourceUnavailable = errors.New("音源接口暂不可用")
)

func sourceWideFailure(err error) bool {
	return errors.Is(err, ErrSourceAuth) || errors.Is(err, ErrSourceRateLimited) || errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrInvalidSourceResponse) || errors.Is(err, context.DeadlineExceeded)
}

// SourceRequestError 仅保留受控类别和退避秒数，不携带脚本错误原文。
type SourceRequestError struct {
	cause      error
	retryAfter time.Duration
}

func (e *SourceRequestError) Error() string             { return e.cause.Error() }
func (e *SourceRequestError) Unwrap() error             { return e.cause }
func (e *SourceRequestError) RetryAfter() time.Duration { return e.retryAfter }
func (e *SourceRequestError) DiagnosticCode() string {
	switch e.cause {
	case ErrNoQuality:
		return "unsupported_quality"
	case ErrSourceAuth:
		return "source_auth"
	case ErrSourceRateLimited:
		return "rate_limited"
	case ErrInvalidSourceResponse:
		return "invalid_response"
	default:
		return "source_error"
	}
}

func classifiedSourceError(code string, retrySeconds int64) error {
	var cause error
	switch code {
	case "ENOQUALITY", "UNSUPPORTED_QUALITY":
		cause = ErrNoQuality
	case "EAUTH", "EUNAUTHORIZED":
		cause = ErrSourceAuth
	case "ERATELIMIT", "RATE_LIMITED":
		cause = ErrSourceRateLimited
	case "EUPSTREAM":
		cause = ErrSourceUnavailable
	case "EINVALIDRESPONSE":
		cause = ErrInvalidSourceResponse
	default:
		return nil
	}
	return &SourceRequestError{cause: cause, retryAfter: time.Duration(min(300, max(1, retrySeconds))) * time.Second}
}
