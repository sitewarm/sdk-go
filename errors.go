package sitewarm

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sitewarm/sdk-go/models"
)

// RateLimit holds the RateLimit-* response headers when present (-1 when absent).
type RateLimit struct {
	Limit, Remaining, Reset int
}

// APIError is returned for every non-2xx answer. It never contains the API key.
type APIError struct {
	Status  int
	Problem models.Problem
	// Code is the problem type slug (for example "site-not-verified"), "" when unknown.
	Code string
	// RetryAfter is the parsed Retry-After, 0 when absent.
	RetryAfter time.Duration
	RateLimit  RateLimit
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("sitewarm API error %d", e.Status)
	if e.Problem.Title != "" {
		s += ": " + e.Problem.Title
	}
	if e.Problem.Detail != nil && *e.Problem.Detail != "" {
		s += " (" + *e.Problem.Detail + ")"
	}
	return s
}

// Sentinel errors for errors.Is. An *APIError matches the sentinel of its status (none for 3xx, 408, 413, 415 and
// other statuses not listed here) and, for two problem types, a more specific one as well.
var (
	ErrAuthentication = errors.New("sitewarm: authentication failed (401)")
	ErrPermission     = errors.New("sitewarm: permission denied (403)")
	ErrNotFound       = errors.New("sitewarm: not found (404)")
	ErrConflict       = errors.New("sitewarm: conflict (409)")
	ErrValidation     = errors.New("sitewarm: validation failed (400/422)")
	ErrRateLimited    = errors.New("sitewarm: rate limited (429)")
	ErrServer         = errors.New("sitewarm: server error (5xx)")

	// ErrIdempotencyKeyReused is a 409 whose problem type is idempotency-key-reused (also matches ErrConflict).
	ErrIdempotencyKeyReused = errors.New("sitewarm: idempotency key reused with a different request (409)")
	// ErrQuotaExceeded is a 422 whose problem type is quota-exceeded (also matches ErrValidation).
	ErrQuotaExceeded = errors.New("sitewarm: quota exceeded (422)")
)

// Is lets callers write errors.Is(err, sitewarm.ErrNotFound).
func (e *APIError) Is(target error) bool {
	if target == ErrIdempotencyKeyReused {
		return e.Status == 409 && e.Code == "idempotency-key-reused"
	}
	if target == ErrQuotaExceeded {
		return e.Status == 422 && e.Code == "quota-exceeded"
	}
	switch {
	case e.Status == 401:
		return target == ErrAuthentication
	case e.Status == 403:
		return target == ErrPermission
	case e.Status == 404:
		return target == ErrNotFound
	case e.Status == 409:
		return target == ErrConflict
	case e.Status == 400 || e.Status == 422:
		return target == ErrValidation
	case e.Status == 429:
		return target == ErrRateLimited
	case e.Status >= 500:
		return target == ErrServer
	}
	return false
}

// ConnectionError means the request never produced an HTTP response. Timeout reports a deadline.
type ConnectionError struct {
	Err     error
	Timeout bool
}

func (e *ConnectionError) Error() string {
	if e.Timeout {
		return "sitewarm: request timed out"
	}
	return "sitewarm: request failed"
}
func (e *ConnectionError) Unwrap() error { return e.Err }

// DecodeError means a 2xx response body was not the expected JSON (for example an HTML page from a proxy).
type DecodeError struct {
	Status int
	Err    error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("sitewarm: invalid JSON in %d response", e.Status)
}
func (e *DecodeError) Unwrap() error { return e.Err }

// ResponseTooLargeError means the response body was larger than the SDK reads (32 MiB). Not retried.
type ResponseTooLargeError struct{ Limit int }

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("sitewarm: response body is larger than %d bytes", e.Limit)
}

// WaitTimeoutError is returned by WarmRuns.Wait; the run keeps going server side.
type WaitTimeoutError struct {
	RunID      string
	LastStatus models.RunStatus
}

func (e *WaitTimeoutError) Error() string {
	return fmt.Sprintf("sitewarm: warm run %s did not finish in time (last status: %s)", e.RunID, e.LastStatus)
}

var slugRE = regexp.MustCompile(`^[a-z0-9-]+$`)

func problemCode(p models.Problem) string {
	if p.Code != nil {
		return string(*p.Code)
	}
	t := strings.TrimRight(p.Type, "/")
	slug := t[strings.LastIndex(t, "/")+1:]
	if slugRE.MatchString(slug) {
		return slug
	}
	return ""
}
