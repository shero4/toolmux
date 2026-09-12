package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// usage observes bounded response fragments without retaining prompts or output.
type usage struct {
	Input, Output    *int64
	Failed, Complete bool
	pending          []byte
	skipping         bool
}

func (u *usage) json(data []byte) {
	var message struct {
		Type    string          `json:"type"`
		Status  string          `json:"status"`
		Error   json.RawMessage `json:"error"`
		Usage   json.RawMessage `json:"usage"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Response struct {
			Usage  json.RawMessage `json:"usage"`
			Status string          `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &message) != nil {
		return
	}
	if len(message.Error) > 0 && string(message.Error) != "null" || message.Type == "error" || message.Type == "response.failed" {
		u.Failed = true
	}
	if message.Type == "response.completed" || message.Type == "message_stop" {
		u.Complete = true
	}
	if message.Type == "response.incomplete" || message.Status == "failed" || message.Status == "incomplete" {
		u.Failed = true
	}
	raw := message.Usage
	if len(raw) == 0 {
		raw = message.Response.Usage
	}
	if len(raw) == 0 {
		raw = message.Message.Usage
	}
	var tokens struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
		Input      *int64 `json:"input_tokens"`
		Output     *int64 `json:"output_tokens"`
	}
	if json.Unmarshal(raw, &tokens) != nil {
		return
	}
	if tokens.Input == nil {
		tokens.Input = tokens.Prompt
	}
	if tokens.Output == nil {
		tokens.Output = tokens.Completion
	}
	if tokens.Input != nil && *tokens.Input >= 0 {
		u.Input = tokens.Input
	}
	if tokens.Output != nil && *tokens.Output >= 0 {
		u.Output = tokens.Output
	}
}

func (u *usage) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			if !u.skipping {
				line := bytes.TrimSpace(u.pending)
				if bytes.HasPrefix(line, []byte("data:")) {
					data := bytes.TrimSpace(line[5:])
					if string(data) == "[DONE]" {
						u.Complete = true
					} else {
						u.json(data)
					}
				}
			}
			u.pending = u.pending[:0]
			u.skipping = false
		} else if !u.skipping {
			if len(u.pending) >= 1<<20 {
				u.pending = u.pending[:0]
				u.skipping = true
			} else {
				u.pending = append(u.pending, b)
			}
		}
	}
	return len(p), nil
}

func isStream(contentType string) bool { return strings.HasPrefix(contentType, "text/event-stream") }
