// Package llm is a minimal OpenAI Chat Completions client for the room's
// own model calls (advisor, warden) through the gateway, which serves that
// API for every model it routes.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls one model through the gateway.
type Client struct {
	// BaseURL is the gateway's base URL; /v1/chat/completions is appended
	// unless it already ends in /v1.
	BaseURL string
	Key     string
	Model   string
	// Effort is passed as reasoning_effort when set.
	Effort string
	HTTP   *http.Client
}

// Message is a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a function call from the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

func (c *Client) url() string {
	u := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasSuffix(u, "/v1") {
		u += "/v1"
	}
	return u + "/chat/completions"
}

// Chat sends messages and returns the assistant's reply.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool) (Message, error) {
	req := map[string]any{"model": c.Model, "messages": msgs}
	if c.Effort != "" {
		req["reasoning_effort"] = c.Effort
	}
	if len(tools) > 0 {
		var ts []any
		for _, t := range tools {
			ts = append(ts, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Parameters,
			}})
		}
		req["tools"] = ts
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(), bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.Key)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := hc.Do(hr)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("gateway: %s: %.300s", resp.Status, b)
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return Message{}, err
	}
	if len(out.Choices) == 0 {
		return Message{}, fmt.Errorf("gateway: no choices")
	}
	return out.Choices[0].Message, nil
}

// JSONObject extracts the first {...} object from text into v.
func JSONObject(text string, v any) error {
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j < i {
		return fmt.Errorf("no JSON object in %.200q", text)
	}
	return json.Unmarshal([]byte(text[i:j+1]), v)
}
