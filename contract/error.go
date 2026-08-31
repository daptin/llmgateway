package contract

import "fmt"

type ErrorCode string

const (
	ErrorInvalidRequest    ErrorCode = "invalid_request"
	ErrorAuthentication    ErrorCode = "authentication_error"
	ErrorPermission        ErrorCode = "permission_error"
	ErrorModelNotFound     ErrorCode = "model_not_found"
	ErrorRateLimit         ErrorCode = "rate_limit_error"
	ErrorInsufficientQuota ErrorCode = "insufficient_quota"
	ErrorTimeout           ErrorCode = "timeout_error"
	ErrorUnavailable       ErrorCode = "service_unavailable"
	ErrorProvider          ErrorCode = "provider_error"
	ErrorInternal          ErrorCode = "internal_error"
)

// Error is safe to expose at a protocol boundary. Cause is deliberately not
// serialized and must only be consumed by redacting telemetry sinks.
type Error struct {
	Code       ErrorCode
	Message    string
	HTTPStatus int
	Retryable  bool
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *Error) Safe() *Error {
	if e == nil {
		return nil
	}
	copy := *e
	copy.Cause = nil
	return &copy
}
