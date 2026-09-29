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
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/tonycoder-hub/cursor-api-proxy/internal/cursor"
)

const maxBodyBytes = 1 << 20
const maxOutputBytes = 8 << 20

type Server struct {
	backend Backend
	keyHash [32]byte
	timeout time.Duration
	slots   chan struct{}
}

func New(backend Backend, apiKey string, timeout time.Duration, maxConcurrent int) (*Server, error) {
	if backend == nil || apiKey == "" || timeout <= 0 || maxConcurrent < 1 {
		return nil, fmt.Errorf("invalid proxy configuration")
	}
	return &Server{backend: backend, keyHash: sha256.Sum256([]byte(apiKey)), timeout: timeout, slots: make(chan struct{}, maxConcurrent)}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	authorization := r.Header.Get("Authorization")
	supplied := strings.TrimPrefix(authorization, "Bearer ")
	candidate := sha256.Sum256([]byte(supplied))
	if !strings.HasPrefix(authorization, "Bearer ") || subtle.ConstantTimeCompare(candidate[:], s.keyHash[:]) != 1 {
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
	if r.URL.Path == "/v1/models" {
		models, err := s.backend.Models(ctx)
		if err != nil {
			upstreamError(w, ctx)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
		return
	}
	s.chat(w, r.WithContext(ctx))
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
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
		upstreamError(w, r.Context())
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
		s.stream(w, r, request, prompt)
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
	if err != nil || !usage.Done {
		upstreamError(w, r.Context())
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

func (s *Server) stream(w http.ResponseWriter, r *http.Request, request chatRequest, prompt string) {
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
	start := func() error {
		if started {
			return nil
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
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
	if err != nil || !usage.Done {
		if !started {
			upstreamError(w, r.Context())
			return
		}
		_ = send(errorValue("upstream_error", "upstream_error", "Cursor upstream stream failed; the completion is incomplete"))
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

func usageValue(usage cursor.TextChunk) map[string]any {
	return map[string]any{"prompt_tokens": usage.PromptTokens, "completion_tokens": usage.CompletionTokens, "total_tokens": usage.PromptTokens + usage.CompletionTokens, "prompt_tokens_details": map[string]int64{"cached_tokens": usage.CachedTokens}}
}

func methodError(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "Method not allowed")
}

func upstreamError(w http.ResponseWriter, ctx context.Context) {
	status, code := http.StatusBadGateway, "upstream_error"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status, code = http.StatusGatewayTimeout, "upstream_timeout"
	}
	writeError(w, status, "upstream_error", code, "Cursor upstream request failed; verify account access and model availability")
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
