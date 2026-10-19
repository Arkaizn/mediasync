package model

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable machine-readable error identifier exposed over IPC.
type ErrorCode string

const (
	ErrSessionNotFound            ErrorCode = "session_not_found"
	ErrNoSessionSelected          ErrorCode = "no_session_selected"
	ErrUnsupported                ErrorCode = "unsupported_capability"
	ErrNotControllable            ErrorCode = "session_not_controllable"
	ErrSessionUnavailable         ErrorCode = "session_unavailable"
	ErrTimeout                    ErrorCode = "timeout"
	ErrProviderError              ErrorCode = "provider_error"
	ErrProviderUnavailable        ErrorCode = "provider_unavailable"
	ErrProviderNotFound           ErrorCode = "provider_not_found"
	ErrAuthFailed                 ErrorCode = "authentication_failed"
	ErrNotAuthenticated           ErrorCode = "not_authenticated"
	ErrInvalidArgument            ErrorCode = "invalid_argument"
	ErrInvalidConfig              ErrorCode = "invalid_config"
	ErrCredentialStore            ErrorCode = "credential_store_error"
	ErrCredentialStoreUnavailable ErrorCode = "credential_store_unavailable"
	ErrServerUnreachable          ErrorCode = "server_unreachable"
	ErrTLS                        ErrorCode = "tls_error"
	ErrInternal                   ErrorCode = "internal_error"
)

// Error is a structured error carrying a stable code.
// Message must never contain secrets.
type Error struct {
	Code    ErrorCode
	Message string
	// Data carries optional structured details safe to expose over IPC.
	Data map[string]any
	// cause is kept for logging/unwrapping only; it is never serialised.
	cause error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.cause }

// Errorf creates a structured error.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap creates a structured error with a cause. The cause's text is not
// included in Message; callers must make sure Message is safe to expose.
func Wrap(code ErrorCode, cause error, message string) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

// WithData attaches structured data.
func (e *Error) WithData(k string, v any) *Error {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.Data[k] = v
	return e
}

// CodeOf extracts the error code, defaulting to ErrInternal.
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ErrInternal
}

// AsError converts any error into a structured *Error.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: ErrInternal, Message: "internal error", cause: err}
}
