package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tonycoder-hub/cursor-api-proxy/internal/cursor"
)

// openKeepAliveStream starts a streaming request against a server whose
// backend waits for release, and returns once the first keep-alive arrived.
func openKeepAliveStream(t *testing.T, server *Server) (*http.Response, *bufio.Reader) {
	t.Helper()
	server.keepAlive = 5 * time.Millisecond
	upstream := httptest.NewServer(server)
	t.Cleanup(upstream.Close)
	request, _ := http.NewRequest("POST", upstream.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer fixture-proxy-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := upstream.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	body := bufio.NewReader(response.Body)
	line, err := body.ReadString('\n')
	if err != nil || line != ": keep-alive\n" {
		t.Fatalf("first line=%q err=%v", line, err)
	}
	return response, body
}

func waitForRelease(ctx context.Context, release <-chan struct{}) error {
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestStreamSendsKeepAliveOnlyBeforeFirstChunk(t *testing.T) {
	release := make(chan struct{})
	backend := &stubBackend{run: func(ctx context.Context, fn func(cursor.TextChunk) error) error {
		if err := waitForRelease(ctx, release); err != nil {
			return err
		}
		if err := fn(cursor.TextChunk{Text: "hello"}); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond) // ten keep-alive intervals
		return fn(cursor.TextChunk{Done: true})
	}}
	server, _ := New(backend, "fixture-proxy-key", 5*time.Second, 1)
	_, body := openKeepAliveStream(t, server)
	close(release)
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rest)
	first := strings.Index(text, "data: ")
	if first < 0 || !strings.Contains(text, `"content":"hello"`) || !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("unexpected stream: %q", text)
	}
	if strings.Contains(text[first:], "keep-alive") {
		t.Fatalf("keep-alive continued after data started: %q", text)
	}
}

func TestStreamFailureAfterKeepAliveIsAnErrorEvent(t *testing.T) {
	release := make(chan struct{})
	backend := &stubBackend{run: func(ctx context.Context, fn func(cursor.TextChunk) error) error {
		if err := waitForRelease(ctx, release); err != nil {
			return err
		}
		return &cursor.StatusError{Op: "RunSSE", Status: 500}
	}}
	server, _ := New(backend, "fixture-proxy-key", 5*time.Second, 1)
	logs := captureLogs(server)
	response, body := openKeepAliveStream(t, server)
	close(release)
	rest, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	requestID := response.Header.Get("X-Request-Id")
	text := string(rest)
	if !strings.Contains(text, `"code":"upstream_error"`) || !strings.Contains(text, "request ID "+requestID) || strings.Contains(text, "[DONE]") || strings.Contains(text, "status 500") {
		t.Fatalf("unexpected stream: %q", text)
	}
	records := logRecords(t, logs)
	if len(records) != 1 || records[0]["request_id"] != requestID || records[0]["status"] != float64(200) || records[0]["partial"] != nil || records[0]["upstream_status"] != float64(500) {
		t.Fatalf("unexpected log records: %s", logs)
	}
}

func TestStreamFailureBeforeKeepAliveKeepsHTTPStatus(t *testing.T) {
	backend := &stubBackend{run: func(context.Context, func(cursor.TextChunk) error) error {
		return &cursor.StatusError{Op: "RunSSE", Status: 500}
	}}
	server, _ := New(backend, "fixture-proxy-key", 5*time.Second, 1)
	captureLogs(server)
	out := call(t, server, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if out.Code != 502 || strings.Contains(out.Body.String(), "keep-alive") {
		t.Fatalf("status=%d body=%q", out.Code, out.Body)
	}
}

// countingWriter is a ResponseWriter that is safe to inspect while a
// keep-alive goroutine writes to it.
type countingWriter struct {
	mu     sync.Mutex
	header http.Header
	writes int
}

func (w *countingWriter) Header() http.Header { return w.header }
func (w *countingWriter) WriteHeader(int)     {}
func (w *countingWriter) Flush()              {}
func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	return len(p), nil
}
func (w *countingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func TestKeepAliveStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &countingWriter{header: http.Header{}}
	stop := startKeepAlive(ctx, w, http.NewResponseController(w), time.Millisecond)
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	for w.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no keep-alive was written")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	time.Sleep(10 * time.Millisecond)
	settled := w.count()
	time.Sleep(20 * time.Millisecond)
	if w.count() != settled {
		t.Fatal("keep-alive continued after the context ended")
	}
	if !stop() || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("keep-alive did not report the committed SSE response")
	}
}

func TestKeepAliveDoesNotWriteAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &countingWriter{header: http.Header{}}
	stop := startKeepAlive(ctx, w, http.NewResponseController(w), time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if stop() || w.count() != 0 || w.Header().Get("Content-Type") != "" {
		t.Fatal("keep-alive wrote after its context was canceled")
	}
}
