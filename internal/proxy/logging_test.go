package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tonycoder-hub/cursor-api-proxy/internal/config"
	"github.com/tonycoder-hub/cursor-api-proxy/internal/cursor"
)

var requestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

const leakyCause = "Authorization: Bearer fixture-bearer-secret token=fixture-token-secret eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.c2ln fixture-cursor-key"

var leakySecrets = []string{"fixture-bearer-secret", "fixture-token-secret", "eyJhbGciOiJub25lIn0", "fixture-cursor-key"}

func captureLogs(s *Server) *bytes.Buffer {
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&buf, nil))
	return &buf
}

func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestUpstreamFailureIsLoggedWithRequestID(t *testing.T) {
	backend := &stubBackend{run: func(context.Context, func(cursor.TextChunk) error) error {
		return fmt.Errorf("%s: %w", leakyCause, &cursor.StatusError{Op: "RunSSE", Status: 401})
	}}
	server, _ := New(backend, "fixture-proxy-key", time.Second, 1)
	server.secret = "fixture-cursor-key"
	logs := captureLogs(server)
	out := call(t, server, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	if out.Code != 502 {
		t.Fatalf("status=%d", out.Code)
	}
	requestID := out.Header().Get("X-Request-Id")
	if !requestIDPattern.MatchString(requestID) {
		t.Fatalf("invalid request ID %q", requestID)
	}
	if !strings.Contains(out.Body.String(), "request ID "+requestID) {
		t.Fatalf("client error is not correlatable: %s", out.Body)
	}
	for _, leaked := range append([]string{"RunSSE", "status 401"}, leakySecrets...) {
		if strings.Contains(out.Body.String(), leaked) {
			t.Fatalf("client error exposed %q: %s", leaked, out.Body)
		}
	}
	records := logRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("log records=%d: %s", len(records), logs)
	}
	record := records[0]
	if record["level"] != "ERROR" || record["request_id"] != requestID || record["status"] != float64(502) || record["model"] != "test-model" ||
		record["upstream_op"] != "RunSSE" || record["upstream_status"] != float64(401) {
		t.Fatalf("unexpected log record: %v", record)
	}
	message, _ := record["error"].(string)
	if !strings.Contains(message, "RunSSE status 401") || !strings.Contains(message, "[REDACTED]") {
		t.Fatalf("log message lacks the sanitized cause: %q", message)
	}
	for _, leaked := range leakySecrets {
		if strings.Contains(logs.String(), leaked) {
			t.Fatalf("log exposed %q: %s", leaked, logs)
		}
	}
}

func TestStreamFailureAfterOutputIsLoggedAsPartial(t *testing.T) {
	backend := &stubBackend{run: func(ctx context.Context, fn func(cursor.TextChunk) error) error {
		if err := fn(cursor.TextChunk{Text: "partial"}); err != nil {
			return err
		}
		return &cursor.TrailerError{Code: "unavailable"}
	}}
	server, _ := New(backend, "fixture-proxy-key", time.Second, 1)
	logs := captureLogs(server)
	out := call(t, server, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	requestID := out.Header().Get("X-Request-Id")
	if out.Code != 200 || !requestIDPattern.MatchString(requestID) || !strings.Contains(out.Body.String(), "request ID "+requestID) {
		t.Fatalf("stream error event is not correlatable: status=%d body=%s", out.Code, out.Body)
	}
	records := logRecords(t, logs)
	if len(records) != 1 || records[0]["request_id"] != requestID || records[0]["partial"] != true || records[0]["status"] != float64(200) || records[0]["upstream_code"] != "unavailable" {
		t.Fatalf("unexpected log records: %s", logs)
	}
}

func TestModelDiscoveryFailureIsLogged(t *testing.T) {
	backend := &stubBackend{modelsErr: &cursor.StatusError{Op: "model discovery", Status: 403}}
	server, _ := New(backend, "fixture-proxy-key", time.Second, 1)
	logs := captureLogs(server)
	out := call(t, server, "GET", "/v1/models", "fixture-proxy-key", "")
	records := logRecords(t, logs)
	if out.Code != 502 || len(records) != 1 || records[0]["request_id"] != out.Header().Get("X-Request-Id") || records[0]["upstream_status"] != float64(403) {
		t.Fatalf("status=%d logs=%s", out.Code, logs)
	}
}

func TestSanitizeErrorRedactsCredentials(t *testing.T) {
	backend := NewCursorBackend(config.Config{CursorAPIKey: "fixture-cursor-key", MaxConcurrent: 1})
	server, _ := New(backend, "fixture-proxy-key", time.Second, 1)
	got := server.sanitizeError(fmt.Errorf(`%s {"accessToken":"fixture-json-token"} api_key=fixture-param-key`, leakyCause))
	for _, leaked := range append([]string{"fixture-json-token", "fixture-param-key"}, leakySecrets...) {
		if strings.Contains(got, leaked) {
			t.Fatalf("sanitized message exposed %q: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "Bearer [REDACTED]") {
		t.Fatalf("unexpected sanitized message: %q", got)
	}
	if got := server.sanitizeError(errors.New(strings.Repeat("x", 4*maxLoggedErrorBytes))); len(got) > maxLoggedErrorBytes+3 {
		t.Fatalf("sanitized message is not bounded: %d bytes", len(got))
	}
}
