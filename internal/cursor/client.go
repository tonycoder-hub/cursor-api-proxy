package cursor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	cursorv1 "github.com/tonycoder-hub/cursor-api-proxy/gen/cursorv1"
)

const (
	defaultRPCBase = "https://api2.cursor.sh"

	procBidiAppend = "/aiserver.v1.BidiService/BidiAppend"
	procRunSSE     = "/agent.v1.AgentService/RunSSE"

	clientType = "sdk"
)

// Client speaks Cursor's agent wire protocol directly.
type Client struct {
	HTTP          *http.Client
	RPCBase       string // defaults to https://api2.cursor.sh
	APIKey        string // exchanged for an access token; never sent to RPCs directly
	ClientVersion string // x-cursor-client-version, e.g. "cli-2025.08.01"
	GhostMode     bool   // x-ghost-mode

	// Env supplies the environment context the server requests mid-turn. If nil, a
	// synthetic context is used without inspecting the host.
	Env *EnvContext

	tok tokenCache // cached access token from exchange_user_api_key
}

// EnvContext is the environment the server asks the client to report before it
// will generate any output. A minimal set (os/cwd/shell) is enough.
type EnvContext struct {
	OSVersion     string
	WorkspacePath string
	Shell         string
	TimeZone      string
}

// NewClient builds a Client with sane defaults.
func NewClient(apiKey string) *Client {
	return &Client{
		HTTP:          http.DefaultClient,
		RPCBase:       defaultRPCBase,
		APIKey:        apiKey,
		ClientVersion: "cli-2025.08.01",
		GhostMode:     true,
	}
}

func (c *Client) base() string {
	if c.RPCBase != "" {
		return strings.TrimRight(c.RPCBase, "/")
	}
	return defaultRPCBase
}

// commonHeaders sets the auth + client identification headers observed on the wire.
// bearer must be the exchanged access token, not the raw API key.
func (c *Client) commonHeaders(h http.Header, bearer string) {
	h.Set("Authorization", "Bearer "+bearer)
	h.Set("x-cursor-client-type", clientType)
	if c.ClientVersion != "" {
		h.Set("x-cursor-client-version", c.ClientVersion)
	}
	if c.GhostMode {
		h.Set("x-ghost-mode", "true")
	}
}

// bidiAppend performs one BidiService/BidiAppend unary call for a client message.
func (c *Client) bidiAppend(ctx context.Context, msg *cursorv1.AgentClientMessage, convID string, seq uint64) error {
	body, err := BuildBidiAppendBody(msg, convID, seq)
	if err != nil {
		return err
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	// BidiAppend is a UNARY Connect call: raw protobuf body, no 5-byte frame prefix.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+procBidiAppend, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.commonHeaders(req.Header, token)
	req.Header.Set("Content-Type", "application/proto")
	req.Header.Set("connect-protocol-version", "1")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("BidiAppend: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("BidiAppend status %d", resp.StatusCode)
	}
	return nil
}

// TextChunk is a decoded piece of the model's output stream.
type TextChunk struct {
	Text     string // assistant text delta
	Thinking string // thinking (reasoning) delta

	// Populated on the final turn-ended frame.
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	Done             bool
}

// IDs carries the conversation identifiers.
type IDs struct {
	ConversationID string // "agent-<uuid>"; run_request.conversation_id
	RunID          string // "<uuid>"; run_request.run_id and the routing key for BidiAppend/RunSSE
}

// Run uploads a single-user-message run request, opens the response stream, and
// answers the server's mid-turn context handshake, invoking onChunk per decoded piece.
func (c *Client) Run(ctx context.Context, model, userText string, ids IDs, onChunk func(TextChunk) error) error {
	runReq := &cursorv1.AgentRunRequest{
		ConversationState: &cursorv1.ConversationStateStructure{},
		Action: &cursorv1.ConversationAction{
			UserMessageAction: &cursorv1.UserMessageAction{
				UserMessage: &cursorv1.UserMessage{Text: userText},
			},
		},
		RequestedModel: &cursorv1.RequestedModel{ModelId: model},
		McpTools:       &cursorv1.McpTools{},
		ConversationId: proto.String(ids.ConversationID),
		RunId:          proto.String(ids.RunID),
	}
	msg := &cursorv1.AgentClientMessage{RunRequest: runReq}

	// seq 0: the initial run_request (no #3 sequence field, per capture).
	if err := c.bidiAppend(ctx, msg, ids.RunID, 0); err != nil {
		return err
	}

	return c.runSSE(ctx, ids, onChunk)
}

// runSSE opens the RunSSE server stream and dispatches decoded chunks, replying to
// the server's context request via additional BidiAppend calls.
func (c *Client) runSSE(ctx context.Context, ids IDs, onChunk func(TextChunk) error) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	body := encodeFrame(BuildRunSSEBody(ids.RunID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+procRunSSE, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.commonHeaders(req.Header, token)
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("x-cursor-streaming", "true")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("RunSSE: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
		return fmt.Errorf("RunSSE status %d: upstream request failed", resp.StatusCode)
	}

	// seq counter shared by the follow-up BidiAppend calls (context reply, acks).
	var seq atomic.Uint64
	seq.Store(1)
	var appendMu sync.Mutex // BidiAppend calls for one conversation must be serialized

	sendClient := func(m *cursorv1.AgentClientMessage) error {
		appendMu.Lock()
		defer appendMu.Unlock()
		return c.bidiAppend(ctx, m, ids.RunID, seq.Add(1)-1)
	}

	r := bufio.NewReader(resp.Body)
	contextSent := false
	blobBytes := 0
	blobs := map[string][]byte{} // remote blob store the server reads/writes mid-turn
	for {
		frame, err := readFrame(r)
		if err == io.EOF {
			return fmt.Errorf("RunSSE ended before end stream")
		}
		if err != nil {
			return err
		}
		if frame.EndStream() {
			return parseRunSSETrailer(frame.Payload)
		}

		var sm cursorv1.AgentServerMessage
		if err := proto.Unmarshal(frame.Payload, &sm); err != nil {
			return fmt.Errorf("decode RunSSE message failed")
		}

		// Answer the mid-turn context request exactly once.
		if !contextSent {
			if esm := sm.GetExecServerMessage(); esm != nil && esm.GetRequestContextArgs() != nil {
				reply := c.contextReply()
				reply.ExecClientMessage.Id = esm.GetId()
				reply.ExecClientMessage.ExecId = esm.GetExecId()
				if err := sendClient(reply); err != nil {
					return fmt.Errorf("context reply: %w", err)
				}
				contextSent = true
			}
		}

		if esm := sm.GetExecServerMessage(); esm != nil && esm.GetRequestContextArgs() == nil {
			return fmt.Errorf("Cursor requested unsupported tool execution")
		}

		// Answer blob store requests so the server can finalize the turn.
		if kv := sm.GetKvServerMessage(); kv != nil {
			reply := &cursorv1.KvClientMessage{Id: kv.GetId()}
			if sb := kv.GetSetBlobArgs(); sb != nil {
				key := string(sb.GetBlobId())
				_, exists := blobs[key]
				nextSize := blobBytes - len(blobs[key]) + len(sb.GetBlobData())
				if !exists {
					nextSize += len(key)
				}
				if (!exists && len(blobs) >= 1024) || nextSize > maxFrameBytes {
					return fmt.Errorf("Cursor blob store limit exceeded")
				}
				blobs[key] = sb.GetBlobData()
				blobBytes = nextSize
				reply.SetBlobResult = &cursorv1.SetBlobResult{}
			} else if gb := kv.GetGetBlobArgs(); gb != nil {
				if data, ok := blobs[string(gb.GetBlobId())]; ok {
					reply.GetBlobResult = &cursorv1.GetBlobResult{BlobData: data}
				} else {
					reply.GetBlobResult = &cursorv1.GetBlobResult{Error: &cursorv1.Error{Message: "blob not found"}}
				}
			}
			if reply.SetBlobResult != nil || reply.GetBlobResult != nil {
				if err := sendClient(&cursorv1.AgentClientMessage{KvClientMessage: reply}); err != nil {
					return fmt.Errorf("kv reply: %w", err)
				}
			}
		}

		if err := emitChunks(&sm, onChunk); err != nil {
			return err
		}
	}
}

type runSSETrailer struct {
	Error *runSSETrailerError `json:"error"`
}

type runSSETrailerError struct {
	Code    string `json:"code"`
	Details []struct {
		Debug *struct {
			Details *struct {
				Title string `json:"title"`
			} `json:"details"`
		} `json:"debug"`
	} `json:"details"`
}

var connectErrorCodes = map[string]struct{}{
	"aborted": {}, "already_exists": {}, "canceled": {}, "data_loss": {},
	"deadline_exceeded": {}, "failed_precondition": {}, "internal": {},
	"invalid_argument": {}, "not_found": {}, "out_of_range": {},
	"permission_denied": {}, "resource_exhausted": {}, "unauthenticated": {},
	"unavailable": {}, "unimplemented": {}, "unknown": {},
}

func parseRunSSETrailer(payload []byte) error {
	var trailer runSSETrailer
	if err := json.Unmarshal(payload, &trailer); err != nil {
		return fmt.Errorf("decode RunSSE end stream failed")
	}
	if trailer.Error == nil {
		return nil
	}

	code := safeConnectCode(trailer.Error.Code)
	modelUnavailable := false
	for _, item := range trailer.Error.Details {
		if item.Debug == nil || item.Debug.Details == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(item.Debug.Details.Title), "Model not available") {
			modelUnavailable = true
			break
		}
	}
	if modelUnavailable {
		return fmt.Errorf("Cursor RunSSE %s: model is not available for this account or region", code)
	}
	return fmt.Errorf("Cursor RunSSE %s: upstream request failed", code)
}

func safeConnectCode(value string) string {
	code := strings.ToLower(strings.TrimSpace(value))
	if _, ok := connectErrorCodes[code]; ok {
		return code
	}
	return "unknown"
}

// contextReply builds the exec_client_message answering request_context_args.
func (c *Client) contextReply() *cursorv1.AgentClientMessage {
	env := c.Env
	if env == nil {
		env = detectEnv()
	}
	return &cursorv1.AgentClientMessage{
		ExecClientMessage: &cursorv1.ExecClientMessage{
			RequestContextResult: &cursorv1.RequestContextResult{
				Success: &cursorv1.RequestContextSuccess{
					RequestContext: &cursorv1.RequestContext{
						Env: &cursorv1.RequestContextEnv{
							OsVersion:      env.OSVersion,
							WorkspacePaths: []string{env.WorkspacePath},
							Shell:          env.Shell,
							TimeZone:       env.TimeZone,
							ProjectFolder:  env.WorkspacePath,
						},
					},
				},
			},
		},
	}
}

// emitChunks pulls text/thinking/usage out of an AgentServerMessage.
func emitChunks(sm *cursorv1.AgentServerMessage, onChunk func(TextChunk) error) error {
	iu := sm.GetInteractionUpdate()
	if iu == nil {
		return nil
	}
	if td := iu.GetTextDelta(); td != nil {
		if t := td.GetText(); t != "" {
			if err := onChunk(TextChunk{Text: t}); err != nil {
				return err
			}
		}
	}
	if thd := iu.GetThinkingDelta(); thd != nil {
		if t := thd.GetText(); t != "" {
			if err := onChunk(TextChunk{Thinking: t}); err != nil {
				return err
			}
		}
	}
	if te := iu.GetTurnEnded(); te != nil {
		return onChunk(TextChunk{
			PromptTokens:     te.GetInputTokens(),
			CompletionTokens: te.GetOutputTokens(),
			CachedTokens:     te.GetCacheReadTokens(),
			Done:             true,
		})
	}
	return nil
}

const maxFrameBytes = 16 << 20

// readFrame reads one Connect frame, decompressing if flagged.
func readFrame(r *bufio.Reader) (Frame, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	flags := hdr[0]
	n := binary.BigEndian.Uint32(hdr[1:5])
	if flags & ^byte(flagCompressed|flagEndStream) != 0 || n > maxFrameBytes {
		return Frame{}, fmt.Errorf("invalid or oversized Connect frame")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	if flags&flagCompressed != 0 {
		zr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return Frame{}, fmt.Errorf("gzip frame: %w", err)
		}
		defer zr.Close()
		dec, err := io.ReadAll(io.LimitReader(zr, maxFrameBytes+1))
		if err != nil {
			return Frame{}, fmt.Errorf("gunzip frame: %w", err)
		}
		if len(dec) > maxFrameBytes {
			return Frame{}, fmt.Errorf("decompressed Connect frame too large")
		}
		payload = dec
	}
	return Frame{Flags: flags, Payload: payload}, nil
}
