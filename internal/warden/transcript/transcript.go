// Package transcript reads the conversation a model request carries, in the
// three API shapes rooms use through the gateway: Anthropic Messages
// (/v1/messages), OpenAI Responses (/v1/responses) and OpenAI Chat
// Completions (/v1/chat/completions). It flattens it into items (user and
// assistant text, tool calls and tool results) and finds the items a room
// has not sent before (Seen).
package transcript

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
)

// API shapes.
const (
	Anthropic = "anthropic"
	Responses = "responses"
	Chat      = "chat"
)

// Kind of an item.
type Kind string

const (
	User       Kind = "user"
	Assistant  Kind = "assistant"
	ToolCall   Kind = "tool_call"
	ToolResult Kind = "tool_result"
)

// MaxText caps an item's text; longer text is cut with a marker.
const MaxText = 4000

// Item is one step of a conversation.
type Item struct {
	Kind Kind
	// Tool is the tool's name (tool calls).
	Tool   string
	CallID string
	// Text is message text, a tool result, or a tool call's input as JSON.
	Text string
	// Input is a tool call's input (JSON object, or a JSON string for custom tools).
	Input   json.RawMessage
	IsError bool
}

// Request is a parsed model request.
type Request struct {
	API   string
	Model string
	Items []Item
}

// APIOf tells the API shape from a request path, or "" if the path carries no
// conversation (for example /v1/models or /v1/messages/count_tokens).
func APIOf(path string) string {
	p := strings.TrimRight(path, "/")
	switch {
	case strings.HasSuffix(p, "/messages"):
		return Anthropic
	case strings.HasSuffix(p, "/responses"):
		return Responses
	case strings.HasSuffix(p, "/chat/completions"):
		return Chat
	}
	return ""
}

// ErrNotConversation is returned for paths without a conversation.
var ErrNotConversation = errors.New("not a conversation request")

// Parse reads the request body sent to path.
func Parse(path string, body []byte) (*Request, error) {
	api := APIOf(path)
	var (
		r   *Request
		err error
	)
	switch api {
	case Anthropic:
		r, err = parseAnthropic(body)
	case Responses:
		r, err = parseResponses(body)
	case Chat:
		r, err = parseChat(body)
	default:
		return nil, ErrNotConversation
	}
	if err != nil {
		return nil, err
	}
	r.API = api
	return r, nil
}

func clip(s string) string {
	if len(s) <= MaxText {
		return s
	}
	return s[:MaxText] + " …[cut]"
}

// text returns the text of a content value: a string, or a list of blocks
// with a "text" field (or nested "content").
func text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Text != "":
			parts = append(parts, b.Text)
		case len(b.Content) > 0:
			if t := text(b.Content); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func message(role, t string) (Item, bool) {
	t = strings.TrimSpace(t)
	if t == "" {
		return Item{}, false
	}
	k := User
	if role == "assistant" {
		k = Assistant
	}
	if role != "user" && role != "assistant" {
		return Item{}, false // system and developer prompts are not the agent's doing
	}
	return Item{Kind: k, Text: clip(t)}, true
}

// ---- Anthropic Messages ----

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func parseAnthropic(body []byte) (*Request, error) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	r := &Request{Model: req.Model}
	for _, m := range req.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			if it, ok := message(m.Role, s); ok {
				r.Items = append(r.Items, it)
			}
			continue
		}
		var blocks []anthropicBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		var txt []string
		flush := func() {
			if it, ok := message(m.Role, strings.Join(txt, "\n")); ok {
				r.Items = append(r.Items, it)
			}
			txt = nil
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				txt = append(txt, b.Text)
			case "tool_use", "server_tool_use":
				flush()
				r.Items = append(r.Items, Item{Kind: ToolCall, Tool: b.Name, CallID: b.ID, Input: b.Input, Text: clip(string(b.Input))})
			case "tool_result":
				flush()
				r.Items = append(r.Items, Item{Kind: ToolResult, CallID: b.ToolUseID, Text: clip(text(b.Content)), IsError: b.IsError})
			}
		}
		flush()
	}
	return r, nil
}

// ---- OpenAI Responses ----

func parseResponses(body []byte) (*Request, error) {
	var req struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	r := &Request{Model: req.Model}
	var s string
	if json.Unmarshal(req.Input, &s) == nil {
		if it, ok := message("user", s); ok {
			r.Items = append(r.Items, it)
		}
		return r, nil
	}
	var items []struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments string          `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Output    json.RawMessage `json:"output"`
		Action    json.RawMessage `json:"action"`
	}
	if len(req.Input) > 0 {
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, err
		}
	}
	for _, it := range items {
		switch it.Type {
		case "message", "":
			if m, ok := message(it.Role, text(it.Content)); ok {
				r.Items = append(r.Items, m)
			}
		case "function_call":
			in := json.RawMessage(it.Arguments)
			if !json.Valid(in) {
				b, _ := json.Marshal(it.Arguments)
				in = b
			}
			r.Items = append(r.Items, Item{Kind: ToolCall, Tool: it.Name, CallID: it.CallID, Input: in, Text: clip(it.Arguments)})
		case "custom_tool_call":
			r.Items = append(r.Items, Item{Kind: ToolCall, Tool: it.Name, CallID: it.CallID, Input: it.Input, Text: clip(text(it.Input))})
		case "local_shell_call":
			in, _ := json.Marshal(map[string]json.RawMessage{"action": it.Action})
			r.Items = append(r.Items, Item{Kind: ToolCall, Tool: "local_shell", CallID: it.CallID, Input: in, Text: clip(string(it.Action))})
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			r.Items = append(r.Items, Item{Kind: ToolResult, CallID: it.CallID, Text: clip(text(it.Output))})
		}
	}
	return r, nil
}

// ---- OpenAI Chat Completions ----

func parseChat(body []byte) (*Request, error) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	r := &Request{Model: req.Model}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			r.Items = append(r.Items, Item{Kind: ToolResult, CallID: m.ToolCallID, Text: clip(text(m.Content))})
			continue
		}
		if it, ok := message(m.Role, text(m.Content)); ok {
			r.Items = append(r.Items, it)
		}
		for _, c := range m.ToolCalls {
			in := json.RawMessage(c.Function.Arguments)
			if !json.Valid(in) {
				b, _ := json.Marshal(c.Function.Arguments)
				in = b
			}
			r.Items = append(r.Items, Item{Kind: ToolCall, Tool: c.Function.Name, CallID: c.ID, Input: in, Text: clip(c.Function.Arguments)})
		}
	}
	return r, nil
}

// ---- deltas ----

// key identifies an item across requests.
type key [16]byte

func (it Item) key() key {
	h := sha256.New()
	for _, s := range []string{string(it.Kind), it.Tool, it.CallID, it.Text} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	var k key
	copy(k[:], h.Sum(nil))
	return k
}

// Seen remembers the items a room has sent, so each request yields only its
// new part. It works for several conversations sharing a key (subagents)
// and survives compaction, at the cost of treating a repeated identical
// message as old. It keeps at most Cap items. Not safe for concurrent use.
type Seen struct {
	Cap  int
	set  map[key]struct{}
	ring []key
	next int
}

// NewSeen returns a Seen holding up to capacity items.
func NewSeen(capacity int) *Seen {
	return &Seen{Cap: capacity, set: map[key]struct{}{}}
}

// Empty reports whether nothing was seen yet.
func (s *Seen) Empty() bool { return len(s.set) == 0 }

// New returns the items not seen before, in order, and marks them seen.
func (s *Seen) New(items []Item) []Item {
	var out []Item
	for _, it := range items {
		k := it.key()
		if _, ok := s.set[k]; ok {
			continue
		}
		s.add(k)
		out = append(out, it)
	}
	return out
}

func (s *Seen) add(k key) {
	if s.Cap <= 0 {
		s.Cap = 4096
	}
	if len(s.ring) < s.Cap {
		s.ring = append(s.ring, k)
	} else {
		delete(s.set, s.ring[s.next])
		s.ring[s.next] = k
		s.next = (s.next + 1) % s.Cap
	}
	s.set[k] = struct{}{}
}
