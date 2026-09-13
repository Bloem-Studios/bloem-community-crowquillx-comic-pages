package silo

import "fmt"

// APIError is an error that can be safely returned to the host HTTP client.
// Its text never contains upstream response bodies, URLs, or credentials.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s (%d)", e.Code, e.Status)
}

func apiError(status int, code string) *APIError {
	return &APIError{Status: status, Code: code}
}

var (
	ErrNotConfigured = apiError(503, "not_configured")
	ErrPreparing     = apiError(202, "preparing")
	ErrCacheStale    = apiError(409, "cache_key_changed")
	ErrCacheMissing  = apiError(409, "cache_evicted")
)
