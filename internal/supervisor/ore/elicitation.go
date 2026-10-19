package ore

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

// MethodElicitation is the app-server's request for an MCP elicitation. ore
// asks whether to run an MCP tool this way (ore 1.161: _meta
// codex_approval_kind "mcp_tool_call", an empty form).
const MethodElicitation = "mcpServer/elicitation/request"

// toolInMessage finds the tool in ore's question ("Allow the github MCP server
// to run tool \"create_issue\"?"); _meta doesn't name it.
var toolInMessage = regexp.MustCompile(`run tool "([^"]+)"`)

// elicitation is what the driver does with one elicitation request.
type elicitation struct {
	// Auto: answer at once with Result (the room's own swarm tools).
	Auto   bool
	Result map[string]any
	// Approval: ask through the approvals inbox, then answer with
	// elicitationResult. Neither set: leave it to the TUI (a real form).
	Approval *agent.Approval
}

// classifyElicitation decides how to answer an elicitation request. Tool
// calls to the swarm server are the room's own messaging tools: they are
// approved for the session without asking anyone (the shipped config also
// pre-approves them, so this is the fallback). Other servers' tool calls go
// to the approvals inbox like commands and patches. Anything else is a form
// for a person and stays in the TUI.
func classifyElicitation(params json.RawMessage) elicitation {
	var p struct {
		ServerName string         `json:"serverName"`
		Mode       string         `json:"mode"`
		Message    string         `json:"message"`
		Meta       map[string]any `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil || p.Mode != "form" {
		return elicitation{}
	}
	if kind, _ := p.Meta["codex_approval_kind"].(string); kind != "mcp_tool_call" {
		return elicitation{}
	}
	if p.ServerName == "swarm" {
		return elicitation{Auto: true, Result: elicitationResult(agent.Decision{Allow: true, ForSession: true})}
	}
	tool := "mcp__" + p.ServerName
	if m := toolInMessage.FindStringSubmatch(p.Message); m != nil {
		tool += "__" + m[1]
	}
	detail := string(params)
	if tp, ok := p.Meta["tool_params"]; ok {
		if b, err := json.Marshal(tp); err == nil {
			detail = string(b)
		}
	}
	return elicitation{Approval: &agent.Approval{
		Tool: tool, Summary: oneLine(strings.TrimSpace(p.Message)), Detail: detail, SessionScope: true,
	}}
}

// elicitationResult is the answer to an MCP tool approval elicitation. An
// approval for the session sets _meta.persist "session" (ore remembers it
// until the thread ends); "always" would write ~/.ore/config.toml.
func elicitationResult(dec agent.Decision) map[string]any {
	if !dec.Allow {
		return map[string]any{"action": "decline", "content": nil, "_meta": nil}
	}
	r := map[string]any{"action": "accept", "content": map[string]any{}, "_meta": nil}
	if dec.ForSession {
		r["_meta"] = map[string]any{"persist": "session"}
	}
	return r
}
