package ore

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

// The request bodies are ore 1.161's mcpServer/elicitation/request for an MCP
// tool call (codex-rs build_mcp_tool_approval_elicitation_request).
func TestClassifyElicitation(t *testing.T) {
	swarm := `{"threadId":"t1","turnId":"u1","serverName":"swarm","mode":"form",
		"message":"Allow the swarm MCP server to run tool \"whoami\"?","requestedSchema":{"type":"object","properties":{}},
		"_meta":{"codex_approval_kind":"mcp_tool_call","persist":["session","always"]}}`
	e := classifyElicitation(json.RawMessage(swarm))
	if !e.Auto || e.Approval != nil {
		t.Fatalf("swarm tool call: %+v", e)
	}
	if want := (map[string]any{"action": "accept", "content": map[string]any{}, "_meta": map[string]any{"persist": "session"}}); !reflect.DeepEqual(e.Result, want) {
		t.Fatalf("swarm answer %v", e.Result)
	}

	other := `{"threadId":"t1","serverName":"github","mode":"form","message":"Allow the github MCP server to run tool \"create_issue\"?",
		"requestedSchema":{"type":"object","properties":{}},
		"_meta":{"codex_approval_kind":"mcp_tool_call","tool_params":{"title":"x"}}}`
	e = classifyElicitation(json.RawMessage(other))
	if e.Auto || e.Approval == nil {
		t.Fatalf("other server: %+v", e)
	}
	if a := e.Approval; a.Tool != "mcp__github__create_issue" || a.Detail != `{"title":"x"}` || !a.SessionScope {
		t.Fatalf("approval %+v", a)
	}

	// A form for a person (no approval kind) stays in the TUI.
	form := `{"threadId":"t1","serverName":"github","mode":"form","message":"Your name?","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}}}}`
	if e := classifyElicitation(json.RawMessage(form)); e.Auto || e.Approval != nil {
		t.Fatalf("form: %+v", e)
	}
}

func TestElicitationResult(t *testing.T) {
	for _, c := range []struct {
		dec    agent.Decision
		action string
		meta   any
	}{
		{agent.Decision{Allow: true}, "accept", nil},
		{agent.Decision{Allow: true, ForSession: true}, "accept", map[string]any{"persist": "session"}},
		{agent.Decision{}, "decline", nil},
	} {
		r := elicitationResult(c.dec)
		if r["action"] != c.action || !reflect.DeepEqual(r["_meta"], c.meta) {
			t.Errorf("%+v: %v", c.dec, r)
		}
	}
}
