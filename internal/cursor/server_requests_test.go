package cursor

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	cursorv1 "github.com/tonycoder-hub/cursor-api-proxy/gen/cursorv1"
)

// requestFixture behaves like Cursor: after each server request it waits for
// the matching client reply before continuing the stream.
func requestFixture(t *testing.T, requests []*cursorv1.AgentServerMessage) *Client {
	t.Helper()
	replies := make(chan *cursorv1.AgentClientMessage, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case procBidiAppend:
			body, _ := io.ReadAll(r.Body)
			_, _, n := protowire.ConsumeTag(body)
			encoded, m := protowire.ConsumeBytes(body[n:])
			raw, err := hex.DecodeString(string(encoded))
			var message cursorv1.AgentClientMessage
			if n < 0 || m < 0 || err != nil || proto.Unmarshal(raw, &message) != nil {
				t.Error("invalid BidiAppend body")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if message.GetRunRequest() == nil {
				replies <- &message
			}
		case procRunSSE:
			flusher := w.(http.Flusher)
			for _, request := range requests {
				payload, _ := proto.Marshal(request)
				_, _ = w.Write(testFrame(0, payload))
				flusher.Flush()
				select {
				case reply := <-replies:
					if exec := request.GetExecServerMessage(); exec != nil {
						got := reply.GetExecClientMessage()
						if got.GetId() != exec.GetId() || got.GetExecId() != exec.GetExecId() || got.GetRequestContextResult() == nil {
							t.Errorf("context reply does not match request %d", exec.GetId())
						}
					}
				case <-r.Context().Done():
					return
				}
			}
			for _, message := range []*cursorv1.AgentServerMessage{
				{InteractionUpdate: &cursorv1.InteractionUpdate{TextDelta: &cursorv1.TextDeltaUpdate{Text: "hello"}}},
				{InteractionUpdate: &cursorv1.InteractionUpdate{TurnEnded: &cursorv1.TurnEndedUpdate{}}},
			} {
				payload, _ := proto.Marshal(message)
				_, _ = w.Write(testFrame(0, payload))
			}
			_, _ = w.Write(testFrame(flagEndStream, []byte(`{}`)))
		}
	}))
	t.Cleanup(server.Close)
	client := NewClient("unused")
	client.RPCBase, client.HTTP = server.URL, server.Client()
	client.tok.accessToken, client.tok.expiresAt = "test-access-token", time.Now().Add(time.Minute)
	return client
}

func runRequests(t *testing.T, requests ...*cursorv1.AgentServerMessage) (string, error) {
	t.Helper()
	client := requestFixture(t, requests)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var text string
	err := client.Run(ctx, "test-model", "hi", IDs{ConversationID: "agent-test", RunID: "test-run"}, func(chunk TextChunk) error {
		text += chunk.Text
		return nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("stream hung waiting for a reply the client never sent")
	}
	return text, err
}

func TestEveryContextRequestIsAnswered(t *testing.T) {
	text, err := runRequests(t,
		&cursorv1.AgentServerMessage{ExecServerMessage: &cursorv1.ExecServerMessage{Id: 7, ExecId: "fixture-exec-1", RequestContextArgs: &cursorv1.RequestContextArgs{}}},
		&cursorv1.AgentServerMessage{ExecServerMessage: &cursorv1.ExecServerMessage{Id: 8, ExecId: "fixture-exec-2", RequestContextArgs: &cursorv1.RequestContextArgs{}}},
	)
	if err != nil || text != "hello" {
		t.Fatalf("text=%q err=%v", text, err)
	}
}

func TestUnsupportedServerRequestsFailTheStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request *cursorv1.AgentServerMessage
		want    string
	}{
		{"exec", &cursorv1.AgentServerMessage{ExecServerMessage: &cursorv1.ExecServerMessage{Id: 5, ExecId: "fixture-exec"}}, "Cursor requested unsupported tool execution"},
		{"kv", &cursorv1.AgentServerMessage{KvServerMessage: &cursorv1.KvServerMessage{Id: 6}}, "Cursor sent an unsupported blob store request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runRequests(t, tc.request)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err=%v want=%q", err, tc.want)
			}
		})
	}
}
