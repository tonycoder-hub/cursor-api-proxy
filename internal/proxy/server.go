// Package proxy exposes the text-chat subset of OpenAI's HTTP API.
package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tonycoder-hub/cursor-api-proxy/internal/cursor"
)

const maxBodyBytes = 1 << 20
const maxOutputBytes = 8 << 20

// keepAliveInterval is how often a stream that has not produced its first
// chunk yet sends an SSE comment, so idle-timeout proxies keep it open.
const keepAliveInterval = 15 * time.Second

type Server struct {
	backend   Backend
	keyHash   [32]byte
	timeout   time.Duration
	slots     chan struct{}
	logger    *slog.Logger
	secret    string // upstream API key, redacted from logs
	keepAlive time.Duration
}

// errIncompleteStream reports an upstream stream that ended without turn-ended.
var errIncompleteStream = errors.New("Cursor stream ended without a turn-ended update")

func New(backend Backend, apiKey string, timeout time.Duration, maxConcurrent int) (*Server, error) {
	if backend == nil || apiKey == "" || timeout <= 0 || maxConcurrent < 1 {
		return nil, fmt.Errorf("invalid proxy configuration")
	}
	s := &Server{backend: backend, keyHash: sha256.Sum256([]byte(apiKey)), timeout: timeout, slots: make(chan struct{}, maxConcurrent), logger: slog.Default(), keepAlive: keepAliveInterval}
	if cursorBackend, ok := backend.(*CursorBackend); ok {
		s.secret = cursorBackend.client.APIKey
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	requestID := newRequestID()
	w.Header().Set("X-Request-Id", requestID)
	supplied, ok := bearerToken(r.Header.Get("Authorization"))
	candidate := sha256.Sum256([]byte(supplied))
	if !ok || subtle.ConstantTimeCompare(candidate[:], s.keyHash[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "A valid proxy Bearer token is required")
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		if r.Method != http.MethodGet {
			methodError(w, "GET")
			return
		}
		// The catalog is cached and its refresh is de-duplicated, so listing
		// models does not take one of the chat concurrency slots.
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		models, err := s.backend.Models(ctx)
		if err != nil {
			s.upstreamError(w, r.WithContext(ctx), requestID, "", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
		return
	case "/v1/chat/completions":
		if r.Method != http.MethodPost {
			methodError(w, "POST")
			return
		}
	default:
		writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "Endpoint not found")
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "busy", "Too many concurrent requests")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	s.chat(w, r.WithContext(ctx), requestID)
}

// bearerToken returns the credential of a Bearer Authorization header. The
// scheme is case-insensitive and may be followed by several spaces (RFC 7235).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimLeft(token, " ")
	return token, token != ""
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request, requestID string) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "invalid_request_error", "content_type", "Content-Type must be application/json")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request chatRequest
	if err := decoder.Decode(&request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "Request exceeds 1 MiB")
			return
		}
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok && len(field) <= 66 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_field", "Unsupported request field "+field+"; see the supported request fields in README")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Invalid request JSON or unsupported field; see the supported request fields in README")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Expected exactly one JSON object")
		return
	}
	prompt, err := request.prompt()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", err.Error())
		return
	}
	catalog, err := s.backend.Models(r.Context())
	if err != nil {
		s.upstreamError(w, r, requestID, "", err)
		return
	}
	known := false
	for _, model := range catalog {
		if model.ID == request.Model {
			known = true
			break
		}
	}
	if !known {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model_not_found", "Model is not in this account's current canonical catalog")
		return
	}
	if request.Stream {
		s.stream(w, r, requestID, request, prompt)
		return
	}
	var text, thinking strings.Builder
	var usage cursor.TextChunk
	size := 0
	err = s.backend.Run(r.Context(), request.Model, prompt, func(chunk cursor.TextChunk) error {
		size += len(chunk.Text) + len(chunk.Thinking)
		if size > maxOutputBytes {
			return fmt.Errorf("output limit exceeded")
		}
		text.WriteString(chunk.Text)
		thinking.WriteString(chunk.Thinking)
		if chunk.Done {
			usage = chunk
		}
		return nil
	})
	if err == nil && !usage.Done {
		err = errIncompleteStream
	}
	if err != nil {
		s.upstreamError(w, r, requestID, request.Model, err)
		return
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if thinking.Len() != 0 {
		message["reasoning_content"] = thinking.String()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "chatcmpl-" + newUUID(), "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}}, "usage": usageValue(usage),
	})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, requestID string, request chatRequest, prompt string) {
	id, created := "chatcmpl-"+newUUID(), time.Now().Unix()
	started := false
	controller := http.NewResponseController(w)
	var usage cursor.TextChunk
	size := 0
	send := func(value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		return controller.Flush()
	}
	chunk := func(delta map[string]string, finish any) map[string]any {
		value := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if request.StreamOptions != nil && request.StreamOptions.IncludeUsage {
			value["usage"] = nil
		}
		return value
	}
	// Until the first chunk, keep-alive comments are the only writer; start
	// stops them before writing data so writes never overlap.
	stopKeepAlive := startKeepAlive(r.Context(), w, controller, s.keepAlive)
	defer stopKeepAlive()
	start := func() error {
		if started {
			return nil
		}
		stopKeepAlive()
		started = true
		setStreamHeaders(w)
		return send(chunk(map[string]string{"role": "assistant", "content": ""}, nil))
	}
	err := s.backend.Run(r.Context(), request.Model, prompt, func(value cursor.TextChunk) error {
		size += len(value.Text) + len(value.Thinking)
		if size > maxOutputBytes {
			return fmt.Errorf("output limit exceeded")
		}
		if value.Done {
			usage = value
		}
		delta := map[string]string{}
		if value.Text != "" {
			delta["content"] = value.Text
		}
		if value.Thinking != "" {
			delta["reasoning_content"] = value.Thinking
		}
		if len(delta) == 0 {
			return nil
		}
		if err := start(); err != nil {
			return err
		}
		return send(chunk(delta, nil))
	})
	if err == nil && !usage.Done {
		err = errIncompleteStream
	}
	if err != nil {
		if committed := stopKeepAlive(); !started && !committed {
			s.upstreamError(w, r, requestID, request.Model, err)
			return
		}
		// Content or keep-alive comments already sent a 200 SSE response, so
		// the failure can only be reported as an error event.
		_, code := upstreamStatus(r)
		message := upstreamMessage(requestID)
		if started {
			message = "Cursor upstream stream failed; the completion is incomplete (request ID " + requestID + ")"
		}
		s.logFailure(r, requestID, request.Model, http.StatusOK, started, err)
		_ = send(errorValue("upstream_error", code, message))
		return
	}
	if err := start(); err != nil {
		return
	}
	if err := send(chunk(map[string]string{}, "stop")); err != nil {
		return
	}
	if request.StreamOptions != nil && request.StreamOptions.IncludeUsage {
		if err := send(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": request.Model, "choices": []any{}, "usage": usageValue(usage)}); err != nil {
			return
		}
	}
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err == nil {
		_ = controller.Flush()
	}
}

func setStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
}

// startKeepAlive writes an SSE comment every interval until the returned stop
// function is called or ctx ends. stop is idempotent, waits for any write in
// progress, and reports whether a keep-alive committed the SSE response.
func startKeepAlive(ctx context.Context, w http.ResponseWriter, controller *http.ResponseController, interval time.Duration) func() bool {
	quit, done := make(chan struct{}), make(chan struct{})
	committed := false
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if ctx.Err() != nil {
				return
			}
			if !committed {
				committed = true
				setStreamHeaders(w)
			}
			_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil || controller.Flush() != nil {
				return
			}
		}
	}()
	var once sync.Once
	return func() bool {
		once.Do(func() { close(quit) })
		<-done
		return committed
	}
}

func usageValue(usage cursor.TextChunk) map[string]any {
	return map[string]any{"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens, "total_tokens": usage.PromptTokens + usage.CompletionTokens, "prompt_tokens_details": map[string]int64{"cached_tokens": usage.CachedTokens}}
}

func methodError(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "Method not allowed")
}

// upstreamError logs the sanitized cause and sends a client error that carries
// only the request ID for correlation, never upstream details.
func (s *Server) upstreamError(w http.ResponseWriter, r *http.Request, requestID, model string, cause error) {
	status, code := upstreamStatus(r)
	s.logFailure(r, requestID, model, status, false, cause)
	writeError(w, status, "upstream_error", code, upstreamMessage(requestID))
}

func upstreamStatus(r *http.Request) (int, string) {
	if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "upstream_timeout"
	}
	return http.StatusBadGateway, "upstream_error"
}

func upstreamMessage(requestID string) string {
	return "Cursor upstream request failed; verify account access and model availability (request ID " + requestID + ")"
}

func errorValue(kind, code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": code}}
}

func writeError(w http.ResponseWriter, status int, kind, code, message string) {
	writeJSON(w, status, errorValue(kind, code, message))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
	_ = controller.Flush()
	// Keep the deadline armed for net/http's final flush. The HTTP server resets
	// connection write deadlines after finishRequest, before the next request.
}
