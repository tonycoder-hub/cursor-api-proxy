// Package cursor implements a direct Go client for Cursor's agent wire protocol.
//
// The transport is NOT the AgentService/Run bidi socket. A normal turn uses two
// plain-HTTP Connect RPCs against https://api2.cursor.sh:
//
//  1. Upload: aiserver.v1.BidiService/BidiAppend (unary), called once per client
//     message. Its request field #1 is a HEX-ENCODED string whose decoded bytes are
//     a serialized cursor.v1.AgentClientMessage; field #2 wraps the conversation
//     UUID ({#1: uuid}); field #3 is an incrementing sequence number.
//  2. Run: agent.v1.AgentService/RunSSE (server-streaming). Request is {#1: uuid}.
//     Response is a stream of Connect frames; each AgentServerMessage.
//
// Connect frame framing: [1 flag byte][4-byte big-endian length][payload].
// flag bit 0x1 => payload gzip-compressed; flag bit 0x2 => end-of-stream JSON trailer.
package cursor

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/proto"

	cursorv1 "github.com/tonycoder-hub/cursor-api-proxy/gen/cursorv1"
)

// --- minimal protobuf wire writers (for the tiny BidiAppend/RunSSE envelopes
// that are not present in the merged proto) ---

// putVarint appends a base-128 varint.
func putVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// tag writes a protobuf tag (field number + wire type).
func tag(b []byte, field, wire int) []byte {
	return putVarint(b, uint64(field)<<3|uint64(wire))
}

// lenField writes a length-delimited (wire type 2) field.
func lenField(b []byte, field int, val []byte) []byte {
	b = tag(b, field, 2)
	b = putVarint(b, uint64(len(val)))
	return append(b, val...)
}

// varintField writes a varint (wire type 0) field.
func varintField(b []byte, field int, v uint64) []byte {
	b = tag(b, field, 0)
	return putVarint(b, v)
}

// uuidWrapper builds the {#1: uuid-string} message used as BidiAppend field #2
// and as the whole RunSSE request body.
func uuidWrapper(convID string) []byte {
	return lenField(nil, 1, []byte(convID))
}

// BuildBidiAppendBody assembles a BidiService/BidiAppend request body carrying one
// AgentClientMessage at the given sequence number for a conversation.
//
// Layout (verified against a live capture):
//
//	#1 string  = hex(marshal(AgentClientMessage))
//	#2 message = {#1: convID}
//	#3 varint  = seq   (omitted when seq == 0; first append had no #3)
func BuildBidiAppendBody(msg *cursorv1.AgentClientMessage, convID string, seq uint64) ([]byte, error) {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal AgentClientMessage: %w", err)
	}
	hexed := hex.EncodeToString(raw)

	var b []byte
	b = lenField(b, 1, []byte(hexed))
	b = lenField(b, 2, uuidWrapper(convID))
	if seq > 0 {
		b = varintField(b, 3, seq)
	}
	return b, nil
}

// BuildRunSSEBody returns the RunSSE request body: {#1: convID}.
func BuildRunSSEBody(convID string) []byte {
	return uuidWrapper(convID)
}

// --- Connect frame codec (for the RunSSE server-streaming response) ---

const (
	flagCompressed = 0x1
	flagEndStream  = 0x2
)

// Frame is one Connect stream frame.
type Frame struct {
	Flags   byte
	Payload []byte // already decompressed if the compressed flag was set
}

// EndStream reports whether this frame is the end-of-stream trailer.
func (f Frame) EndStream() bool { return f.Flags&flagEndStream != 0 }

// encodeFrame writes a single Connect frame (never compressed; used for requests).
func encodeFrame(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}
