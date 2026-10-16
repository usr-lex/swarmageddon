// Package transcript turns a room's session transcript into simple items for
// the read-only Agent tab (docs/design.md §8.4): Claude Code's session JSONL
// (~/.claude/projects/<work dir>/<session>.jsonl) and ore's rollout JSONL
// (~/.ore/sessions/YYYY/MM/DD/rollout-*.jsonl). Both formats are internal to
// their CLIs; unknown lines are skipped. Texts are clipped, and nothing is
// added that the transcript doesn't hold.
package transcript

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Roles of an item.
const (
	RoleUser       = "user"
	RoleAssistant  = "assistant"
	RoleTool       = "tool"
	RoleToolResult = "tool_result"
	RoleSystem     = "system"
)

// Limits on what an item carries.
const (
	maxText   = 4000
	maxInput  = 500
	maxResult = 800
)

// Item is one entry of the rendered transcript.
type Item struct {
	ID         string
	At         time.Time
	Role       string
	Text       string
	ToolName   string
	ToolInput  string
	ToolResult string
}

// Parser parses the lines of one transcript file, in order. It remembers
// tool call names so results can say which tool they belong to.
type Parser struct {
	// CLI is claude or ore.
	CLI   string
	line  int
	tools map[string]string // call id -> tool name
}

// Parse returns the items of one line.
func (p *Parser) Parse(line []byte) []Item {
	p.line++
	if p.tools == nil {
		p.tools = map[string]string{}
	}
	if p.CLI == "ore" {
		return p.ore(line)
	}
	return p.claude(line)
}

// Clip shortens s to n bytes, on valid UTF-8, marking the cut.
func Clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n - 3
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// oneLine collapses whitespace.
func oneLine(s string, n int) string { return Clip(strings.Join(strings.Fields(s), " "), n) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// summarizeInput renders a tool input (JSON) on one line: the command or
// path when there is one, else the compact JSON.
func summarizeInput(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"command", "cmd", "file_path", "path", "pattern", "url", "query", "description"} {
			switch v := m[k].(type) {
			case string:
				if v != "" {
					return oneLine(v, maxInput)
				}
			case []any:
				var parts []string
				for _, x := range v {
					parts = append(parts, fmt.Sprint(x))
				}
				if len(parts) > 0 {
					return oneLine(strings.Join(parts, " "), maxInput)
				}
			}
		}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		// ore passes function arguments as a JSON string.
		if strings.HasPrefix(strings.TrimSpace(s), "{") {
			return summarizeInput(json.RawMessage(s))
		}
		return oneLine(s, maxInput)
	}
	return oneLine(string(raw), maxInput)
}

// textOf flattens a string or a list of content blocks with text fields.
func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// claude parses a Claude Code session entry: {"type":"user"|"assistant"|
// "system", "uuid", "timestamp", "message":{"role","content"}}.
func (p *Parser) claude(line []byte) []Item {
	var e struct {
		Type      string `json:"type"`
		UUID      string `json:"uuid"`
		Timestamp string `json:"timestamp"`
		IsMeta    bool   `json:"isMeta"`
		Content   string `json:"content"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	id := e.UUID
	if id == "" {
		id = fmt.Sprintf("L%d", p.line)
	}
	at := parseTime(e.Timestamp)
	switch e.Type {
	case "system":
		if e.Content == "" {
			return nil
		}
		return []Item{{ID: id, At: at, Role: RoleSystem, Text: Clip(e.Content, maxText)}}
	case "user", "assistant":
	default:
		return nil
	}
	if e.IsMeta {
		return nil
	}
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []Item{{ID: id, At: at, Role: e.Type, Text: Clip(s, maxText)}}
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
	}
	if json.Unmarshal(e.Message.Content, &blocks) != nil {
		return nil
	}
	var out []Item
	for i, b := range blocks {
		bid := fmt.Sprintf("%s#%d", id, i)
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, Item{ID: bid, At: at, Role: e.Type, Text: Clip(b.Text, maxText)})
			}
		case "tool_use":
			p.tools[b.ID] = b.Name
			out = append(out, Item{ID: bid, At: at, Role: RoleTool, ToolName: b.Name, ToolInput: summarizeInput(b.Input)})
		case "tool_result":
			res := textOf(b.Content)
			if b.IsError {
				res = "error: " + res
			}
			out = append(out, Item{ID: bid, At: at, Role: RoleToolResult, ToolName: p.tools[b.ToolUseID], ToolResult: Clip(res, maxResult)})
		}
	}
	return out
}

// ore parses a rollout line: {"timestamp", "type":"session_meta"|
// "response_item"|..., "payload":{...}}. Only response items and the
// session start are shown; event_msg lines repeat them.
func (p *Parser) ore(line []byte) []Item {
	var e struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	id := fmt.Sprintf("L%d", p.line)
	at := parseTime(e.Timestamp)
	switch e.Type {
	case "session_meta":
		var m struct {
			ID  string `json:"id"`
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(e.Payload, &m)
		return []Item{{ID: id, At: at, Role: RoleSystem, Text: Clip("session "+m.ID+" started in "+m.Cwd, maxText)}}
	case "response_item":
	default:
		return nil
	}
	var r struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		CallID    string          `json:"call_id"`
		Output    json.RawMessage `json:"output"`
		Action    struct {
			Command []string `json:"command"`
		} `json:"action"`
	}
	if json.Unmarshal(e.Payload, &r) != nil {
		return nil
	}
	switch r.Type {
	case "message":
		if r.Role != RoleUser && r.Role != RoleAssistant {
			return nil // developer and system messages: instructions, not conversation
		}
		t := textOf(r.Content)
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []Item{{ID: id, At: at, Role: r.Role, Text: Clip(t, maxText)}}
	case "function_call", "custom_tool_call":
		p.tools[r.CallID] = r.Name
		in := r.Arguments
		if r.Type == "custom_tool_call" {
			in = r.Input
		}
		return []Item{{ID: id, At: at, Role: RoleTool, ToolName: r.Name, ToolInput: summarizeInput(in)}}
	case "local_shell_call":
		p.tools[r.CallID] = "shell"
		return []Item{{ID: id, At: at, Role: RoleTool, ToolName: "shell", ToolInput: oneLine(strings.Join(r.Action.Command, " "), maxInput)}}
	case "function_call_output", "custom_tool_call_output":
		name := r.Name
		if name == "" {
			name = p.tools[r.CallID]
		}
		return []Item{{ID: id, At: at, Role: RoleToolResult, ToolName: name, ToolResult: Clip(outputText(r.Output), maxResult)}}
	}
	return nil
}

// outputText reads a function call output: a string, a list of content
// items, or an object with a content or output string.
func outputText(raw json.RawMessage) string {
	if t := textOf(raw); t != "" {
		return t
	}
	var o struct {
		Content string `json:"content"`
		Output  string `json:"output"`
	}
	if json.Unmarshal(raw, &o) == nil {
		if o.Content != "" {
			return o.Content
		}
		return o.Output
	}
	return ""
}

// Script is the read-only shell script that finds the newest transcript
// and follows it. It runs as the agent user (runuser) with sh -c; $1 is the
// Claude project directory name, $2 the number of lines to start with.
// Without a transcript it waits for one to appear.
const Script = `set -u
find_claude() { ls -1t "$HOME/.claude/projects/$1"/*.jsonl 2>/dev/null | head -n 1; }
find_any_claude() { ls -1t "$HOME"/.claude/projects/*/*.jsonl 2>/dev/null | head -n 1; }
find_ore() { ls -1t "$HOME"/.ore/sessions/*/*/*/rollout-*.jsonl 2>/dev/null | head -n 1; }
while :; do
  case "$3" in
    ore) f=$(find_ore) ;;
    *) f=$(find_claude "$1"); [ -n "$f" ] || f=$(find_any_claude) ;;
  esac
  if [ -n "$f" ]; then exec tail -n "$2" -F "$f"; fi
  sleep 5
done`

// ProjectDir is Claude Code's directory name for a working directory: the
// path with every character that isn't a letter or digit replaced by "-".
func ProjectDir(cwd string) string {
	b := []byte(cwd)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// Command is the exec command for a room's agent user: the transcript of
// cli (claude or ore) whose session runs in cwd (an absolute path), starting
// with tail lines.
func Command(user, cli, cwd string, tail int) []string {
	return []string{"runuser", "-u", user, "--", "env", "HOME=/home/" + user, "sh", "-c", Script, "transcript",
		ProjectDir(strings.TrimRight(cwd, "/")), fmt.Sprint(tail), cli}
}
