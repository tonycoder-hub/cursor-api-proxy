package cursor

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	cursorv1 "github.com/tonycoder-hub/cursor-api-proxy/gen/cursorv1"
)

func TestRunSSEErrorTrailerReturnsSafeUpstreamError(t *testing.T) {
	payload := []byte(`{
  "error": {
    "code": "resource_exhausted",
    "message": "SECRET_MESSAGE_MUST_NOT_BE_EXPOSED",
    "details": [{
      "debug": {
        "details": {
          "title": "Model not available",
          "detail": "SECRET_DETAIL_MUST_NOT_BE_EXPOSED"
        }
      },
      "value": "OPAQUE_VALUE_MUST_NOT_BE_EXPOSED"
    }]
  }
}`)

	err := runSSEWithTrailer(t, payload)
	if err == nil {
		t.Fatal("RunSSE returned success for an error trailer")
	}
	got := err.Error()
	want := "Cursor RunSSE resource_exhausted: model is not available for this account or region"
	if got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	for _, secret := range []string{"SECRET_MESSAGE", "SECRET_DETAIL", "OPAQUE_VALUE"} {
		if strings.Contains(got, secret) {
			t.Fatalf("error exposed upstream field %q: %q", secret, got)
		}
	}
}

func TestRunSSEEmptyTrailerSucceeds(t *testing.T) {
	if err := runSSEWithTrailer(t, []byte(`{}`)); err != nil {
		t.Fatalf("RunSSE empty trailer: %v", err)
	}
}

func TestRunSSEMalformedTrailerFails(t *testing.T) {
	err := runSSEWithTrailer(t, []byte(`{"error":"SECRET_MALFORMED_TRAILER"`))
	if err == nil || err.Error() != "decode RunSSE end stream failed" {
		t.Fatalf("RunSSE malformed trailer error = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET_MALFORMED_TRAILER") {
		t.Fatalf("error exposed malformed trailer bytes: %q", err)
	}
}

func TestRunSSENonOKResponseDoesNotExposeBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"SECRET_UPSTREAM_BODY"}`))
	}))
	defer server.Close()

	client := NewClient("unused")
	client.HTTP = server.Client()
	client.RPCBase = server.URL
	client.tok.accessToken = "test-access-token"
	client.tok.fetchedAt = time.Now()

	err := client.runSSE(context.Background(), IDs{RunID: "test-run"}, func(TextChunk) error {
		t.Fatal("non-OK response emitted a text chunk")
		return nil
	})
	if err == nil {
		t.Fatal("RunSSE returned success for a non-OK response")
	}
	if got, want := err.Error(), "RunSSE status 502: upstream request failed"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "SECRET_UPSTREAM_BODY") {
		t.Fatalf("error exposed upstream response body: %q", err)
	}
}

func TestRunSSEErrorTrailerDoesNotExposeUnknownMetadata(t *testing.T) {
	payload := []byte(`{"error":{"code":"SECRET_CODE","message":"SECRET_MESSAGE","details":[{"debug":{"details":{"title":"SECRET_TITLE","detail":"SECRET_DETAIL"}},"value":"SECRET_VALUE"}]}}`)
	err := runSSEWithTrailer(t, payload)
	if err == nil {
		t.Fatal("RunSSE returned success for an error trailer")
	}
	if got, want := err.Error(), "Cursor RunSSE unknown: upstream request failed"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestRunSSEKnownCodeUsesGenericMessage(t *testing.T) {
	err := runSSEWithTrailer(t, []byte(`{"error":{"code":"unavailable","message":"SECRET_MESSAGE"}}`))
	if err == nil || err.Error() != "Cursor RunSSE unavailable: upstream request failed" {
		t.Fatalf("RunSSE generic error = %v", err)
	}
}

func TestRunSSEImmediateEOFFails(t *testing.T) {
	err := runSSEWithFrames(t, nil, func(TextChunk) error {
		t.Fatal("empty stream emitted a text chunk")
		return nil
	})
	if err == nil || err.Error() != "RunSSE ended before end stream" {
		t.Fatalf("RunSSE EOF error = %v", err)
	}
}

func TestRunSSEEOFAfterTextFails(t *testing.T) {
	payload, err := proto.Marshal(&cursorv1.AgentServerMessage{
		InteractionUpdate: &cursorv1.InteractionUpdate{
			TextDelta: &cursorv1.TextDeltaUpdate{Text: "partial"},
		},
	})
	if err != nil {
		t.Fatalf("marshal text frame: %v", err)
	}
	var got string
	err = runSSEWithFrames(t, [][]byte{testFrame(0, payload)}, func(chunk TextChunk) error {
		got += chunk.Text
		return nil
	})
	if got != "partial" {
		t.Fatalf("partial text = %q", got)
	}
	if err == nil || err.Error() != "RunSSE ended before end stream" {
		t.Fatalf("RunSSE partial EOF error = %v", err)
	}
}

func runSSEWithTrailer(t *testing.T, payload []byte) error {
	t.Helper()
	return runSSEWithFrames(t, [][]byte{testFrame(flagEndStream, payload)}, func(TextChunk) error {
		t.Fatal("end stream trailer emitted a text chunk")
		return nil
	})
}

func runSSEWithFrames(t *testing.T, frames [][]byte, onChunk func(TextChunk) error) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != procRunSSE {
			t.Fatalf("request path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		for _, frame := range frames {
			_, _ = w.Write(frame)
		}
	}))
	defer server.Close()

	client := NewClient("unused")
	client.HTTP = server.Client()
	client.RPCBase = server.URL
	client.tok.accessToken = "test-access-token"
	client.tok.fetchedAt = time.Now()

	return client.runSSE(context.Background(), IDs{RunID: "test-run"}, onChunk)
}

func testFrame(flags byte, payload []byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}
