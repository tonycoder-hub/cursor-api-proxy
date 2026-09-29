package cursor

import (
	"encoding/hex"
	"testing"

	"google.golang.org/protobuf/proto"

	cursorv1 "github.com/tonycoder-hub/cursor-api-proxy/gen/cursorv1"
)

func TestBidiAppendRoundTrip(t *testing.T) {
	// Build the same message our client would send.
	msg := &cursorv1.AgentClientMessage{
		RunRequest: &cursorv1.AgentRunRequest{
			Action: &cursorv1.ConversationAction{
				UserMessageAction: &cursorv1.UserMessageAction{
					UserMessage: &cursorv1.UserMessage{Text: "Reply with exactly: PING_OK"},
				},
			},
			RequestedModel: &cursorv1.RequestedModel{ModelId: "composer-2.5"},
			ConversationId: proto.String("00000000-0000-4000-8000-000000000000"),
		},
	}
	body, err := BuildBidiAppendBody(msg, "00000000-0000-4000-8000-000000000000", 0)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	// The body's field #1 is hex(marshal(msg)); decode it back and re-unmarshal.
	f1hex := extractField1String(t, body)
	raw, err := hex.DecodeString(f1hex)
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	var got cursorv1.AgentClientMessage
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal inner: %v", err)
	}

	rr := got.GetRunRequest()
	if rr == nil {
		t.Fatal("no run_request")
	}
	if txt := rr.GetAction().GetUserMessageAction().GetUserMessage().GetText(); txt != "Reply with exactly: PING_OK" {
		t.Errorf("user text = %q", txt)
	}
	if m := rr.GetRequestedModel().GetModelId(); m != "composer-2.5" {
		t.Errorf("model = %q", m)
	}
	if c := rr.GetConversationId(); c != "00000000-0000-4000-8000-000000000000" {
		t.Errorf("conv id = %q", c)
	}
}

// extractField1String reads the length-delimited field #1 (tag 0x0a) from body.
func extractField1String(t *testing.T, body []byte) string {
	t.Helper()
	if len(body) < 1 || body[0] != 0x0a {
		t.Fatalf("field1 tag not found, got 0x%02x", body[0])
	}
	i := 1
	// read varint length
	var n, shift uint64
	for {
		b := body[i]
		i++
		n |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	return string(body[i : i+int(n)])
}
