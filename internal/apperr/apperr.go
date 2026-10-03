// Package apperr defines the typed errors shared by the API, store and
// worker layers, and their mapping to RFC 9457 problem+json responses.
package apperr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Kind classifies an error independent of where it was raised.
type Kind int

const (
	KindInternal Kind = iota
	KindValidation
	KindUnauthorized
	KindForbidden
	KindNotFound
	KindConflict
	KindPreconditionFailed
	KindPayloadTooLarge
	KindRateLimited
	KindUnavailable
	KindUnprocessable
	KindUnsupportedMedia
)

// FieldError describes a single invalid input field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error is the application error type. Detail is safe to show to clients;
// the wrapped cause is only ever logged.
type Error struct {
	Kind       Kind
	Code       string // stable machine-readable slug, e.g. "idempotency-key-reused"
	Detail     string
	Fields     []FieldError
	RetryAfter time.Duration
	Cause      error
}

func (e *Error) Error() string {
	msg := e.Code
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func New(kind Kind, code, detail string) *Error {
	return &Error{Kind: kind, Code: code, Detail: detail}
}

func Validation(detail string, fields ...FieldError) *Error {
	return &Error{Kind: KindValidation, Code: "validation-failed", Detail: detail, Fields: fields}
}

func Unauthorized(detail string) *Error {
	return &Error{Kind: KindUnauthorized, Code: "unauthorized", Detail: detail}
}

func Forbidden(code, detail string) *Error {
	return &Error{Kind: KindForbidden, Code: code, Detail: detail}
}

func NotFound(detail string) *Error {
	return &Error{Kind: KindNotFound, Code: "not-found", Detail: detail}
}

func Conflict(code, detail string) *Error {
	return &Error{Kind: KindConflict, Code: code, Detail: detail}
}

func PreconditionFailed(detail string) *Error {
	return &Error{Kind: KindPreconditionFailed, Code: "precondition-failed", Detail: detail}
}

func RateLimited(retryAfter time.Duration) *Error {
	return &Error{Kind: KindRateLimited, Code: "rate-limited", Detail: "too many requests", RetryAfter: retryAfter}
}

func Unavailable(detail string, cause error) *Error {
	return &Error{Kind: KindUnavailable, Code: "service-unavailable", Detail: detail, Cause: cause, RetryAfter: 5 * time.Second}
}

// Internal wraps an unexpected failure. The cause is logged, never returned.
func Internal(cause error) *Error {
	return &Error{Kind: KindInternal, Code: "internal-error", Detail: "an internal error occurred", Cause: cause}
}

// Status returns the HTTP status for the error kind.
func (e *Error) Status() int {
	switch e.Kind {
	case KindValidation:
		return http.StatusBadRequest
	case KindUnprocessable:
		return http.StatusUnprocessableEntity
	case KindUnauthorized:
		return http.StatusUnauthorized
	case KindForbidden:
		return http.StatusForbidden
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindPreconditionFailed:
		return http.StatusPreconditionFailed
	case KindPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case KindRateLimited:
		return http.StatusTooManyRequests
	case KindUnavailable:
		return http.StatusServiceUnavailable
	case KindUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	default:
		return http.StatusInternalServerError
	}
}

// As coerces any error into an *Error; unknown errors become Internal.
func As(err error) *Error {
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	return Internal(err)
}

// problem is the RFC 9457 wire format.
type problem struct {
	Type      string       `json:"type"`
	Title     string       `json:"title"`
	Status    int          `json:"status"`
	Detail    string       `json:"detail,omitempty"`
	Errors    []FieldError `json:"errors,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
}

// Write renders err as problem+json. 5xx details never include the cause.
func Write(w http.ResponseWriter, err *Error, requestID string) {
	status := err.Status()
	p := problem{
		Type:      "https://smtp-handler.dev/problems/" + err.Code,
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    err.Detail,
		Errors:    err.Fields,
		RequestID: requestID,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if err.RetryAfter > 0 {
		secs := int(err.RetryAfter.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// Errorf is a convenience for internal errors with formatting.
func Errorf(format string, args ...any) *Error {
	return Internal(fmt.Errorf(format, args...))
}
