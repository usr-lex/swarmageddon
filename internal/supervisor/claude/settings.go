// Package claude drives Claude Code in a room (ADR 0006) and renders the
// platform's managed settings for the operator.
package claude

import (
	"encoding/json"
	"fmt"
	"time"
)

// HookEvents are the events the platform hooks, each calling `swarm hook <Event>`.
var HookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"Stop", "Notification", "PermissionRequest",
}

// ManagedPolicy is the input for ManagedSettings and ManagedMCP: the persona's
// Claude Code policy plus platform settings. It has no Kubernetes types so the
// operator can fill it from a RoleTemplate.
type ManagedPolicy struct {
	// Allow and Deny are permission rules, e.g. "Bash(npm test)".
	Allow []string
	Deny  []string
	// ManagedRulesOnly sets allowManagedPermissionRulesOnly.
	ManagedRulesOnly bool
	// DefaultMode sets permissions.defaultMode (default, acceptEdits, auto,
	// plan, bypassPermissions); empty leaves it unset.
	DefaultMode string
	// DisableBypass sets permissions.disableBypassPermissionsMode to "disable".
	DisableBypass bool
	// ExtraHooks is the persona's "hooks" object in settings.json format; its
	// entries are added after the platform's.
	ExtraHooks json.RawMessage
	// ApprovalTimeout is the PermissionRequest hook timeout (default 24 h).
	ApprovalTimeout time.Duration
	// SwarmCommand is the swarm binary (default "swarm", found on PATH).
	SwarmCommand string
	// Env goes into the managed "env" block (e.g. ANTHROPIC_BASE_URL). Never secrets.
	Env map[string]string
	// CleanupPeriodDays is the transcript retention; 0 leaves the default.
	CleanupPeriodDays int
	// ExtraMCPServers are added to managed-mcp.json next to swarm, in .mcp.json format.
	ExtraMCPServers map[string]json.RawMessage
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type hookMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

func (p ManagedPolicy) swarm() string {
	if p.SwarmCommand != "" {
		return p.SwarmCommand
	}
	return "swarm"
}

// ManagedSettings renders /etc/claude-code/managed-settings.json.
func ManagedSettings(p ManagedPolicy) ([]byte, error) {
	timeout := p.ApprovalTimeout
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	hooks := map[string][]json.RawMessage{}
	for _, ev := range HookEvents {
		h := hookCommand{Type: "command", Command: p.swarm() + " hook " + ev, Timeout: 30}
		if ev == "PermissionRequest" {
			h.Timeout = int(timeout / time.Second)
		}
		b, _ := json.Marshal(hookMatcher{Hooks: []hookCommand{h}})
		hooks[ev] = append(hooks[ev], b)
	}
	if len(p.ExtraHooks) > 0 && string(p.ExtraHooks) != "null" {
		var extra map[string][]json.RawMessage
		if err := json.Unmarshal(p.ExtraHooks, &extra); err != nil {
			return nil, fmt.Errorf("persona hooks: %w", err)
		}
		for ev, ms := range extra {
			hooks[ev] = append(hooks[ev], ms...)
		}
	}
	perms := map[string]any{}
	if len(p.Allow) > 0 {
		perms["allow"] = p.Allow
	}
	if len(p.Deny) > 0 {
		perms["deny"] = p.Deny
	}
	if p.DefaultMode != "" {
		perms["defaultMode"] = p.DefaultMode
	}
	if p.DisableBypass {
		perms["disableBypassPermissionsMode"] = "disable"
	}
	s := map[string]any{
		"hooks":                 hooks,
		"allowManagedHooksOnly": true,
		"crossSessionInbound":   "accept",
	}
	if len(perms) > 0 {
		s["permissions"] = perms
	}
	if p.ManagedRulesOnly {
		s["allowManagedPermissionRulesOnly"] = true
	}
	if len(p.Env) > 0 {
		s["env"] = p.Env
	}
	if p.CleanupPeriodDays > 0 {
		s["cleanupPeriodDays"] = p.CleanupPeriodDays
	}
	return json.MarshalIndent(s, "", "  ")
}

// ManagedMCP renders /etc/claude-code/managed-mcp.json with the swarm stdio
// server. managedMcpServers in managed settings only takes HTTP and SSE
// servers, so the stdio server goes here. Note that this file takes exclusive
// control: the agent can't add MCP servers of its own.
func ManagedMCP(p ManagedPolicy) ([]byte, error) {
	servers := map[string]any{
		"swarm": map[string]any{"type": "stdio", "command": p.swarm(), "args": []string{"mcp"}},
	}
	for name, raw := range p.ExtraMCPServers {
		if name == "swarm" {
			return nil, fmt.Errorf("mcp server name swarm is reserved")
		}
		servers[name] = raw
	}
	return json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
}
