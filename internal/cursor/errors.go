package cursor

import (
	"fmt"
	"net/http"
)

// StatusError reports a non-OK HTTP status from a Cursor endpoint. It never
// carries the upstream response body, so it is safe to log.
type StatusError struct {
	Op     string // endpoint or RPC, e.g. "RunSSE"
	Status int    // HTTP status code returned by Cursor
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s status %d: upstream request failed", e.Op, e.Status)
}

// TrailerError reports a Connect end-stream error from RunSSE. Code is limited
// to the documented Connect codes; upstream messages and details are dropped.
type TrailerError struct {
	Code             string
	ModelUnavailable bool
}

func (e *TrailerError) Error() string {
	if e.ModelUnavailable {
		return fmt.Sprintf("Cursor RunSSE %s: model is not available for this account or region", e.Code)
	}
	return fmt.Sprintf("Cursor RunSSE %s: upstream request failed", e.Code)
}

// authRejected reports whether status means Cursor refused the bearer token.
func authRejected(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}
