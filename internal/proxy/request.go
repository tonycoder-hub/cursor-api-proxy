package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	N *int `json:"n,omitempty"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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
	var prompt strings.Builder
	hasUser := false
	for _, message := range r.Messages {
		switch message.Role {
		case "system", "developer", "user", "assistant":
		default:
			return "", fmt.Errorf("only system, developer, user and assistant roles are supported")
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
