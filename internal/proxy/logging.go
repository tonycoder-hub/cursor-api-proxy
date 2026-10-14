package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/tonycoder-hub/cursor-api-proxy/internal/cursor"
)

const maxLoggedErrorBytes = 512

var (
	authSchemePattern  = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	jwtPattern         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)
	secretParamPattern = regexp.MustCompile(`(?i)\b((?:api[_-]?)?key|(?:access[_-]?|refresh[_-]?)?token|secret|password)(["']?\s*[:=]\s*["']?)[^\s"'&,;]+`)
)

// newRequestID returns the ID that correlates a client response with server logs.
func newRequestID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return "req_" + hex.EncodeToString(id[:])
}

// sanitizeError renders cause for logs: configured credentials and
// credential-shaped substrings are redacted and the result is bounded.
func (s *Server) sanitizeError(cause error) string {
	if cause == nil {
		return ""
	}
	message := cause.Error()
	if s.secret != "" {
		message = strings.ReplaceAll(message, s.secret, "[REDACTED]")
	}
	message = authSchemePattern.ReplaceAllString(message, "$1 [REDACTED]")
	message = jwtPattern.ReplaceAllString(message, "[REDACTED]")
	message = secretParamPattern.ReplaceAllString(message, "$1$2[REDACTED]")
	if len(message) > maxLoggedErrorBytes {
		message = strings.ToValidUTF8(message[:maxLoggedErrorBytes], "") + "..."
	}
	return message
}

// logFailure records a failed request server-side under the request ID that was
// returned to the client. Upstream status and Connect code are logged as fields.
func (s *Server) logFailure(r *http.Request, requestID, model string, status int, partial bool, cause error) {
	attrs := []any{"request_id", requestID, "method", r.Method, "path", r.URL.Path, "status", status}
	if model != "" {
		attrs = append(attrs, "model", model)
	}
	if partial {
		attrs = append(attrs, "partial", true)
	}
	var statusErr *cursor.StatusError
	if errors.As(cause, &statusErr) {
		attrs = append(attrs, "upstream_op", statusErr.Op, "upstream_status", statusErr.Status)
	}
	var trailerErr *cursor.TrailerError
	if errors.As(cause, &trailerErr) {
		attrs = append(attrs, "upstream_code", trailerErr.Code)
	}
	attrs = append(attrs, "error", s.sanitizeError(cause))
	if errors.Is(r.Context().Err(), context.Canceled) {
		s.logger.Info("request canceled before completion", attrs...)
		return
	}
	s.logger.Error("upstream request failed", attrs...)
}
