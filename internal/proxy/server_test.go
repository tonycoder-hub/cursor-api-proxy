package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cursorv1 "github.com/tonycoder-hub/cursor-api-proxy/gen/cursorv1"
	"github.com/tonycoder-hub/cursor-api-proxy/internal/config"
	"github.com/tonycoder-hub/cursor-api-proxy/internal/cursor"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func frame(flags byte, message []byte) []byte {
	data := make([]byte, 5+len(message))
	data[0] = flags
	binary.BigEndian.PutUint32(data[1:5], uint32(len(message)))
	copy(data[5:], message)
	return data
}

func messageFrame(t *testing.T, message *cursorv1.AgentServerMessage) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return frame(0, data)
}

func call(t *testing.T, handler http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	return out
}

func TestHTTPToCursorWireProtocol(t *testing.T) {
	var models, exchanges, contextReplies, blobReplies atomic.Int32
	var prompt atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			models.Add(1)
			if r.Header.Get("Authorization") != "Bearer fixture-cursor-key" {
				t.Error("model discovery used the wrong credential")
			}
			io.WriteString(w, `{"items":[{"id":"test-model","aliases":["auto"]},{"id":"test-model"},{"id":""}]}`)
		case "/auth/exchange_user_api_key":
			exchanges.Add(1)
			if r.Header.Get("Authorization") != "Bearer fixture-cursor-key" {
				t.Error("key exchange used the wrong credential")
			}
			io.WriteString(w, `{"accessToken":"fixture-upstream-token"}`)
		case "/aiserver.v1.BidiService/BidiAppend":
			if r.Header.Get("Authorization") != "Bearer fixture-upstream-token" {
				t.Error("RPC did not use the exchanged token")
			}
			if r.Header.Get("Content-Type") != "application/proto" {
				t.Error("invalid BidiAppend content type")
			}
			body, _ := io.ReadAll(r.Body)
			num, kind, n := protowire.ConsumeTag(body)
			if n < 0 || num != 1 || kind != protowire.BytesType {
				t.Error("invalid BidiAppend envelope")
				w.WriteHeader(400)
				return
			}
			encoded, n := protowire.ConsumeBytes(body[n:])
			if n < 0 {
				t.Error("invalid BidiAppend string")
				w.WriteHeader(400)
				return
			}
			raw, err := hex.DecodeString(string(encoded))
			if err != nil {
				t.Error("invalid inner hex")
				w.WriteHeader(400)
				return
			}
			var message cursorv1.AgentClientMessage
			if proto.Unmarshal(raw, &message) != nil {
				t.Error("invalid inner protobuf")
				w.WriteHeader(400)
				return
			}
			if run := message.GetRunRequest(); run != nil {
				prompt.Store(run.GetAction().GetUserMessageAction().GetUserMessage().GetText())
				if run.GetRequestedModel().GetModelId() != "test-model" {
					t.Error("wrong model sent to wire protocol")
				}
			}
			if reply := message.GetExecClientMessage(); reply != nil {
				contextReplies.Add(1)
				if reply.GetId() != 7 || reply.GetExecId() != "fixture-exec" {
					t.Error("context correlation was lost")
				}
				env := reply.GetRequestContextResult().GetSuccess().GetRequestContext().GetEnv()
				if env.GetProjectFolder() != "/workspace" {
					t.Error("context is not synthetic")
				}
			}
			if kv := message.GetKvClientMessage(); kv != nil {
				blobReplies.Add(1)
				if kv.GetId() == 10 && string(kv.GetGetBlobResult().GetBlobData()) != "fixture-data" {
					t.Error("blob round trip failed")
				}
			}
		case "/agent.v1.AgentService/RunSSE":
			if r.Header.Get("Authorization") != "Bearer fixture-upstream-token" {
				t.Error("stream did not use the exchanged token")
			}
			body, _ := io.ReadAll(r.Body)
			if len(body) < 6 || body[0] != 0 || int(binary.BigEndian.Uint32(body[1:5])) != len(body)-5 {
				t.Error("invalid RunSSE framing")
			}
			w.Header().Set("Content-Type", "application/connect+proto")
			for _, message := range []*cursorv1.AgentServerMessage{
				{ExecServerMessage: &cursorv1.ExecServerMessage{Id: 7, ExecId: "fixture-exec", RequestContextArgs: &cursorv1.RequestContextArgs{}}},
				{KvServerMessage: &cursorv1.KvServerMessage{Id: 9, SetBlobArgs: &cursorv1.SetBlobArgs{BlobId: []byte("fixture-blob"), BlobData: []byte("fixture-data")}}},
				{KvServerMessage: &cursorv1.KvServerMessage{Id: 10, GetBlobArgs: &cursorv1.GetBlobArgs{BlobId: []byte("fixture-blob")}}},
				{InteractionUpdate: &cursorv1.InteractionUpdate{ThinkingDelta: &cursorv1.ThinkingDeltaUpdate{Text: "reason"}}},
				{InteractionUpdate: &cursorv1.InteractionUpdate{TextDelta: &cursorv1.TextDeltaUpdate{Text: "hello"}}},
				{InteractionUpdate: &cursorv1.InteractionUpdate{TurnEnded: &cursorv1.TurnEndedUpdate{InputTokens: proto.Int64(12), OutputTokens: proto.Int64(3), CacheReadTokens: proto.Int64(4)}}},
			} {
				w.Write(messageFrame(t, message))
			}
			w.Write(frame(2, []byte(`{}`)))
		default:
			t.Error("unexpected upstream path")
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	backend := NewCursorBackend(config.Config{CursorAPIKey: "fixture-cursor-key", RPCBaseURL: upstream.URL, ModelsURL: upstream.URL + "/v1/models", MaxConcurrent: 4})
	server, err := New(backend, "fixture-proxy-key", time.Second, 4)
	if err != nil {
		t.Fatal(err)
	}
	out := call(t, server, "GET", "/v1/models", "fixture-proxy-key", "")
	if out.Code != 200 || strings.Count(out.Body.String(), `"id":"test-model"`) != 1 || strings.Contains(out.Body.String(), "auto") {
		t.Fatalf("invalid canonical catalog: status=%d body=%s", out.Code, out.Body)
	}
	out = call(t, server, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"test-model","messages":[{"role":"system","content":"be concise"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if out.Code != 200 {
		t.Fatalf("completion status=%d body=%s", out.Code, out.Body)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "hello" || completion.Choices[0].Message.Reasoning != "reason" || completion.Usage.Total != 15 {
		t.Fatalf("unexpected completion: %s", out.Body)
	}
	if prompt.Load() != "[system]\nbe concise\n\n[user]\nhi" {
		t.Error("history prompt was not preserved")
	}
	out = call(t, server, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)
	if out.Code != 200 || out.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("invalid stream status %d", out.Code)
	}
	if !strings.HasSuffix(out.Body.String(), "data: [DONE]\n\n") || !strings.Contains(out.Body.String(), `"choices":[]`) || !strings.Contains(out.Body.String(), `"cached_tokens":4`) {
		t.Fatalf("invalid stream: %s", out.Body)
	}
	if models.Load() != 1 || exchanges.Load() != 1 || contextReplies.Load() != 2 || blobReplies.Load() != 4 {
		t.Fatalf("unexpected upstream counts: models=%d exchange=%d context=%d blobs=%d", models.Load(), exchanges.Load(), contextReplies.Load(), blobReplies.Load())
	}
}

type stubBackend struct {
	run        func(context.Context, func(cursor.TextChunk) error) error
	modelCalls atomic.Int32
}

func (b *stubBackend) Models(context.Context) ([]Model, error) {
	b.modelCalls.Add(1)
	return []Model{{ID: "test-model"}}, nil
}
func (b *stubBackend) Run(ctx context.Context, _ string, _ string, fn func(cursor.TextChunk) error) error {
	return b.run(ctx, fn)
}

func TestRequestRejectionBeforeUpstream(t *testing.T) {
	backend := &stubBackend{}
	server, _ := New(backend, "fixture-proxy-key", time.Second, 1)
	cases := []struct {
		name, body, key string
		status          int
	}{
		{"authentication", `{}`, "wrong", 401},
		{"malformed", `{`, "fixture-proxy-key", 400},
		{"trailing", `{} {}`, "fixture-proxy-key", 400},
		{"model required", `{"messages":[{"role":"user","content":"hi"}]}`, "fixture-proxy-key", 400},
		{"tools", `{"model":"test-model","tools":[]}`, "fixture-proxy-key", 400},
		{"sampling", `{"model":"test-model","temperature":0}`, "fixture-proxy-key", 400},
		{"image", `{"model":"test-model","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image"}}]}]}`, "fixture-proxy-key", 400},
		{"null content", `{"model":"test-model","messages":[{"role":"user","content":null}]}`, "fixture-proxy-key", 400},
		{"multiple choices", `{"model":"test-model","n":2,"messages":[{"role":"user","content":"hi"}]}`, "fixture-proxy-key", 400},
		{"oversized", `{"model":"` + strings.Repeat("a", maxBodyBytes) + `"}`, "fixture-proxy-key", 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := call(t, server, "POST", "/v1/chat/completions", tc.key, tc.body)
			if out.Code != tc.status {
				t.Fatalf("status=%d expected=%d", out.Code, tc.status)
			}
		})
	}
	if backend.modelCalls.Load() != 0 {
		t.Fatal("invalid request reached upstream")
	}
	if out := call(t, server, "GET", "/healthz", "", ""); out.Code != 200 {
		t.Fatal("healthz requires authentication")
	}
	if out := call(t, server, "GET", "/v1/chat/completions", "fixture-proxy-key", ""); out.Code != 405 {
		t.Fatal("wrong method accepted")
	}
	if out := call(t, server, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`); out.Code != 400 {
		t.Fatal("unknown model accepted")
	}
}

func TestStreamFailureNeverEmitsSuccessfulFinish(t *testing.T) {
	for _, stage := range []string{"before", "after-text", "after-turn-ended"} {
		t.Run(stage, func(t *testing.T) {
			backend := &stubBackend{run: func(ctx context.Context, fn func(cursor.TextChunk) error) error {
				if stage != "before" {
					if err := fn(cursor.TextChunk{Text: "partial"}); err != nil {
						return err
					}
				}
				if stage == "after-turn-ended" {
					if err := fn(cursor.TextChunk{Done: true}); err != nil {
						return err
					}
				}
				return fmt.Errorf("fixture-private-upstream-detail")
			}}
			server, _ := New(backend, "fixture-proxy-key", time.Second, 1)
			out := call(t, server, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`)
			if stage == "before" && out.Code != 502 {
				t.Fatalf("pre-stream status=%d", out.Code)
			}
			if stage != "before" && out.Code != 200 {
				t.Fatalf("partial stream status=%d", out.Code)
			}
			for _, forbidden := range []string{`"finish_reason":"stop"`, "[DONE]", "fixture-private-upstream-detail"} {
				if strings.Contains(out.Body.String(), forbidden) {
					t.Fatalf("stream exposed %s", forbidden)
				}
			}
			if !strings.Contains(out.Body.String(), `"error"`) {
				t.Fatal("missing error event")
			}
		})
	}
}

func TestCancellationAndConcurrencyLimit(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	backend := &stubBackend{run: func(ctx context.Context, fn func(cursor.TextChunk) error) error {
		close(entered)
		if err := fn(cursor.TextChunk{Text: "partial"}); err != nil {
			return err
		}
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}}
	handler, _ := New(backend, "fixture-proxy-key", time.Second, 1)
	server := httptest.NewServer(handler)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer fixture-proxy-key")
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("upstream not started")
	}
	out := call(t, handler, "GET", "/v1/models", "fixture-proxy-key", "")
	if out.Code != 429 {
		t.Fatalf("concurrency status=%d", out.Code)
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel upstream")
	}
}

func TestUpstreamRedirectDoesNotForwardCredentials(t *testing.T) {
	var called atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1) }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	backend := NewCursorBackend(config.Config{CursorAPIKey: "fixture-cursor-key", ModelsURL: redirect.URL, MaxConcurrent: 1})
	_, err := backend.Models(context.Background())
	if err == nil || called.Load() != 0 {
		t.Fatal("upstream redirect was followed")
	}
}

func TestEmptyCatalogFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"items":[]}`) }))
	defer upstream.Close()
	backend := NewCursorBackend(config.Config{CursorAPIKey: "fixture-cursor-key", ModelsURL: upstream.URL, MaxConcurrent: 1})
	if _, err := backend.Models(context.Background()); err == nil {
		t.Fatal("empty catalog accepted")
	}
}

func TestTimeoutAndMissingTurnEnded(t *testing.T) {
	for _, mode := range []string{"timeout", "missing-turn"} {
		t.Run(mode, func(t *testing.T) {
			backend := &stubBackend{run: func(ctx context.Context, fn func(cursor.TextChunk) error) error {
				if mode == "timeout" {
					<-ctx.Done()
					return ctx.Err()
				}
				return fn(cursor.TextChunk{Text: "partial"})
			}}
			handler, _ := New(backend, "fixture-proxy-key", 20*time.Millisecond, 1)
			out := call(t, handler, "POST", "/v1/chat/completions", "fixture-proxy-key", `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
			want := 502
			if mode == "timeout" {
				want = 504
			}
			if out.Code != want {
				t.Fatalf("status=%d want=%d", out.Code, want)
			}
			if bytes.Contains(out.Body.Bytes(), []byte(`"finish_reason":"stop"`)) {
				t.Fatal("incomplete result succeeded")
			}
		})
	}
}

func TestCatalogRefreshWaiterCanCancel(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"items":[{"id":"test-model"}]}`)
	}))
	defer upstream.Close()
	defer close(release)
	backend := NewCursorBackend(config.Config{CursorAPIKey: "fixture-cursor-key", ModelsURL: upstream.URL, MaxConcurrent: 2})
	done := make(chan error, 1)
	go func() { _, err := backend.Models(context.Background()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("catalog refresh did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	canceled := make(chan error, 1)
	go func() { _, err := backend.Models(ctx); canceled <- err }()
	select {
	case err := <-canceled:
		if err != context.DeadlineExceeded {
			t.Fatalf("waiter error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("catalog waiter did not observe cancellation")
	}
}
