package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// chatRequest lists every accepted field; unknown fields are rejected. The
// README documents which OpenAI parameters are ignored or only accepted with
// their default value.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage       bool `json:"include_usage"`
		IncludeObfuscation bool `json:"include_obfuscation"` // ignored
	} `json:"stream_options,omitempty"`
	N *int `json:"n,omitempty"`

	// Accepted for OpenAI client compatibility and ignored: the Cursor wire
	// protocol has no equivalent. Values must still have the OpenAI JSON type.
	Temperature         *float64 `json:"temperature"`
	TopP                *float64 `json:"top_p"`
	PresencePenalty     *float64 `json:"presence_penalty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty"`
	Seed                *int64   `json:"seed"`
	User                *string  `json:"user"`
	MaxTokens           *int64   `json:"max_tokens"`
	MaxCompletionTokens *int64   `json:"max_completion_tokens"`
	ParallelToolCalls   *bool    `json:"parallel_tool_calls"`

	// Accepted only with values that request the default behavior; anything
	// else is rejected because ignoring it would change the response contract.
	Stop           json.RawMessage `json:"stop"`
	ResponseFormat json.RawMessage `json:"response_format"`
	Tools          json.RawMessage `json:"tools"`
	ToolChoice     json.RawMessage `json:"tool_choice"`
	Functions      json.RawMessage `json:"functions"`
	FunctionCall   json.RawMessage `json:"function_call"`
	Logprobs       *bool           `json:"logprobs"`
	TopLogprobs    *int            `json:"top_logprobs"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// Accepted and ignored; reasoning_content lets clients send back this
	// proxy's own assistant messages.
	Name             *string `json:"name"`
	ReasoningContent *string `json:"reasoning_content"`
	// Accepted only when empty, as OpenAI SDKs echo them in assistant history.
	Refusal      json.RawMessage `json:"refusal"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
}

// checkDefaults rejects accepted OpenAI fields whose value asks for behavior
// this proxy cannot provide.
func (r *chatRequest) checkDefaults() error {
	switch {
	case !emptyJSON(r.Stop):
		return fmt.Errorf("stop sequences are not supported")
	case !emptyJSON(r.Tools) || !emptyJSON(r.Functions):
		return fmt.Errorf("tool calling is not supported")
	case !noToolChoice(r.ToolChoice) || !noToolChoice(r.FunctionCall):
		return fmt.Errorf("tool_choice must be none or auto; tool calling is not supported")
	case !textResponseFormat(r.ResponseFormat):
		return fmt.Errorf("only response_format type text is supported")
	case r.Logprobs != nil && *r.Logprobs || r.TopLogprobs != nil && *r.TopLogprobs != 0:
		return fmt.Errorf("logprobs are not supported")
	}
	return nil
}

// emptyJSON reports whether raw is absent, null, an empty string or an empty array.
func emptyJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value := value.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	}
	return false
}

func noToolChoice(raw json.RawMessage) bool {
	var choice string
	return emptyJSON(raw) || json.Unmarshal(raw, &choice) == nil && (choice == "none" || choice == "auto")
}

func textResponseFormat(raw json.RawMessage) bool {
	var format struct {
		Type string `json:"type"`
	}
	return emptyJSON(raw) || json.Unmarshal(raw, &format) == nil && format.Type == "text"
}

func (r *chatRequest) prompt() (string, error) {
	r.Model = strings.TrimSpace(r.Model)
	if r.Model == "" {
		return "", fmt.Errorf("model is required; select a canonical ID from /v1/models")
	}
	if len(r.Messages) == 0 || len(r.Messages) > 1024 {
		return "", fmt.Errorf("messages must contain between 1 and 1024 entries")
	}
	if r.N != nil && *r.N != 1 {
		return "", fmt.Errorf("only n=1 is supported")
	}
	if r.StreamOptions != nil && !r.Stream {
		return "", fmt.Errorf("stream_options requires stream=true")
	}
	if err := r.checkDefaults(); err != nil {
		return "", err
	}
	var prompt strings.Builder
	hasUser := false
	for _, message := range r.Messages {
		switch message.Role {
		case "system", "developer", "user", "assistant":
		default:
			return "", fmt.Errorf("only system, developer, user and assistant roles are supported")
		}
		if !emptyJSON(message.ToolCalls) || !emptyJSON(message.FunctionCall) || !emptyJSON(message.Refusal) {
			return "", fmt.Errorf("tool calls and refusals in message history are not supported")
		}
		text, err := contentText(message.Content)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(text) == "" {
			return "", fmt.Errorf("message content must not be empty")
		}
		if message.Role == "user" {
			hasUser = true
		}
		fmt.Fprintf(&prompt, "[%s]\n%s\n\n", message.Role, text)
	}
	if !hasUser {
		return "", fmt.Errorf("at least one user message is required")
	}
	return strings.TrimSpace(prompt.String()), nil
}

func contentText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", fmt.Errorf("message content is required")
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text, nil
		}
	}
	if raw[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&parts) == nil && len(parts) > 0 {
			var text strings.Builder
			for _, part := range parts {
				if part.Type != "text" {
					return "", fmt.Errorf("only text content parts are supported")
				}
				text.WriteString(part.Text)
			}
			return text.String(), nil
		}
	}
	return "", fmt.Errorf("content must be a string or an array of text parts")
}
