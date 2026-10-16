// Package mockmodel is a scripted model server for tests (design §14). It
// speaks Anthropic Messages (/v1/messages), OpenAI Responses (/v1/responses)
// and Chat Completions (/v1/chat/completions), streaming and not, and replays
// scripted turns keyed by a marker substring in the conversation's latest
// user text.
package mockmodel

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Script is a list of turns plus the default reply.
type Script struct {
	// Default is the reply when no turn matches (default "ok").
	Default string `json:"default,omitempty"`
	Turns   []Turn `json:"turns"`
}

// Turn replays Steps when the latest user text containing a marker contains Match.
type Turn struct {
	Match string `json:"match"`
	// Steps are consecutive model responses within one turn: step i is
	// returned when i tool outputs follow the matching user message. Every
	// step but the last should call a tool.
	Steps []Step `json:"steps"`
}

// Step is one model response. Exactly one of Text, Command, Write, MCP or
// Tool is set; Text may accompany a tool call.
type Step struct {
	Text string `json:"text,omitempty"`
	// Command runs a shell command (Claude: Bash; ore: exec_command).
	Command string `json:"command,omitempty"`
	// Write creates a file (Claude: Write; ore: apply_patch).
	Write *WriteFile `json:"write,omitempty"`
	// MCP calls an MCP tool, e.g. server swarm, tool send_message.
	MCP *MCPCall `json:"mcp,omitempty"`
	// Tool is a raw tool call by name.
	Tool *ToolCall `json:"tool,omitempty"`
	// Delay before the response is sent, e.g. "2s".
	Delay Duration `json:"delay,omitempty"`
}

// WriteFile creates path with content.
type WriteFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// MCPCall names an MCP server's tool.
type MCPCall struct {
	Server string         `json:"server"`
	Tool   string         `json:"tool"`
	Input  map[string]any `json:"input,omitempty"`
}

// ToolCall is a tool call by its exact name.
type ToolCall struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input,omitempty"`
}

// Duration parses "2s" style strings.
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// LoadScript reads a YAML or JSON script.
func LoadScript(file string) (*Script, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return ParseScript(b)
}

// ParseScript parses YAML or JSON.
func ParseScript(b []byte) (*Script, error) {
	var s Script
	if err := yaml.UnmarshalStrict(b, &s); err != nil {
		return nil, err
	}
	for i, t := range s.Turns {
		if t.Match == "" {
			return nil, fmt.Errorf("turn %d: empty match", i)
		}
	}
	return &s, nil
}

func (s *Script) defaultText() string {
	if s.Default != "" {
		return s.Default
	}
	return "ok"
}

// conversation is a request normalized across the three APIs.
type conversation struct {
	msgs  []convMsg
	tools []toolDef
}

type convMsg struct {
	role        string // user, assistant, tool
	texts       []string
	toolOutputs int
}

type toolDef struct {
	name   string
	custom bool // freeform (Responses "custom" tool)
}

func (c *conversation) hasTool(name string) (toolDef, bool) {
	for _, t := range c.tools {
		if t.name == name {
			return t, true
		}
	}
	return toolDef{}, false
}

// pick finds the step for a conversation: the latest user text containing
// any turn's marker selects the turn, and the tool outputs after it the step.
func (s *Script) pick(c *conversation) (turn string, step *Step, index int) {
	if len(c.tools) == 0 {
		return "", nil, 0
	}
	for i := len(c.msgs) - 1; i >= 0; i-- {
		m := c.msgs[i]
		if m.role != "user" {
			continue
		}
		text := strings.Join(m.texts, "\n")
		// The latest marker in the text wins, so a steer with a new marker
		// switches turns.
		best, bestPos := -1, -1
		for ti, t := range s.Turns {
			if p := strings.LastIndex(text, t.Match); p > bestPos {
				best, bestPos = ti, p
			}
		}
		if best < 0 {
			continue
		}
		n := 0
		for _, after := range c.msgs[i+1:] {
			n += after.toolOutputs
		}
		t := s.Turns[best]
		if n >= len(t.Steps) {
			return t.Match, nil, n
		}
		return t.Match, &t.Steps[n], n
	}
	return "", nil, 0
}
