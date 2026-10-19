// Package ore drives ore through its app-server (ADR 0007) and renders the
// platform's /etc/codex files for the operator.
package ore

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"sort"

	"github.com/BurntSushi/toml"
)

// GatewayKeyEnv is the variable holding the room's gateway key for ore.
const GatewayKeyEnv = "SWARM_GATEWAY_KEY"

// Policy is the input for Requirements and Config: the persona's ore policy
// plus platform settings. No Kubernetes types, so the operator can fill it
// from a RoleTemplate and Room.
type Policy struct {
	// ApprovalPolicy: on-request (default), on-failure or never.
	ApprovalPolicy string
	// ApprovalsReviewer: user (default) or auto_review.
	ApprovalsReviewer string
	// SandboxMode: read-only, workspace-write (default) or danger-full-access.
	SandboxMode string
	// Bypass runs without approvals or sandbox (never + danger-full-access).
	Bypass bool
	// MCPTools: per MCP server, the enabled tools; empty means all. The
	// "swarm" entry restricts the platform's own server.
	MCPTools map[string][]string
	// ExtraMCPServers are allowed besides swarm: name -> command.
	ExtraMCPServers map[string]string
	// GatewayURL is the model gateway's base URL (e.g. http://gateway...:4000/v1).
	GatewayURL string
	// WireAPI: responses (default), chat or anthropic.
	WireAPI string
	Model   string
	Effort  string
	// SwarmCommand is the swarm binary (default "swarm").
	SwarmCommand string
	// SwarmEnv is passed to `swarm mcp` (e.g. SWARM_SOCKET in tests).
	SwarmEnv map[string]string
	// SwarmToolsApproval is swarm's default_tools_approval_mode (default
	// "approve"); the canary sets "prompt" to test the driver's answer.
	SwarmToolsApproval string
}

func (p Policy) approval() string {
	switch {
	case p.Bypass:
		return "never"
	case p.ApprovalPolicy != "":
		return p.ApprovalPolicy
	}
	return "on-request"
}

func (p Policy) reviewer() string {
	if p.ApprovalsReviewer != "" {
		return p.ApprovalsReviewer
	}
	return "user"
}

func (p Policy) sandbox() string {
	switch {
	case p.Bypass:
		return "danger-full-access"
	case p.SandboxMode != "":
		return p.SandboxMode
	}
	return "workspace-write"
}

func (p Policy) swarm() string {
	if p.SwarmCommand != "" {
		return p.SwarmCommand
	}
	return "swarm"
}

// Provider is the gateway model provider table.
func (p Policy) Provider() map[string]any {
	wire := p.WireAPI
	if wire == "" {
		wire = "responses"
	}
	m := map[string]any{"name": "gateway", "env_key": GatewayKeyEnv, "wire_api": wire}
	if p.GatewayURL != "" {
		m["base_url"] = p.GatewayURL
	}
	return m
}

// Requirements renders /etc/codex/requirements.toml: the allowed approval
// policy, reviewer and sandbox mode, the gateway provider, and the allowed
// MCP servers (swarm plus ExtraMCPServers).
func Requirements(p Policy) ([]byte, error) {
	servers := map[string]any{
		"swarm": map[string]any{"identity": map[string]any{"command": p.swarm()}},
	}
	for name, cmd := range p.ExtraMCPServers {
		if name == "swarm" {
			return nil, fmt.Errorf("mcp server name swarm is reserved")
		}
		servers[name] = map[string]any{"identity": map[string]any{"command": cmd}}
	}
	r := map[string]any{
		"allowed_approval_policies":   []string{p.approval()},
		"allowed_approvals_reviewers": []string{p.reviewer()},
		"allowed_sandbox_modes":       sandboxModes(p.sandbox()),
		"model_provider":              "gateway",
		"model_providers":             map[string]any{"gateway": p.Provider()},
		"mcp_servers":                 servers,
	}
	return encode(r)
}

// Config renders /etc/codex/config.toml: the persona's defaults and the
// swarm MCP server with its tool allowlist.
func Config(p Policy) ([]byte, error) {
	swarm := map[string]any{
		"command":                     p.swarm(),
		"args":                        []string{"mcp"},
		"default_tools_approval_mode": cmp.Or(p.SwarmToolsApproval, "approve"),
	}
	if tools := p.MCPTools["swarm"]; len(tools) > 0 && (len(tools) != 1 || tools[0] != "*") {
		swarm["enabled_tools"] = tools
	}
	if len(p.SwarmEnv) > 0 {
		swarm["env"] = p.SwarmEnv
	}
	c := map[string]any{
		"approval_policy":    p.approval(),
		"approvals_reviewer": p.reviewer(),
		"sandbox_mode":       p.sandbox(),
		"model_provider":     "gateway",
		"model_providers":    map[string]any{"gateway": p.Provider()},
		"mcp_servers":        map[string]any{"swarm": swarm},
	}
	if p.Model != "" {
		c["model"] = p.Model
	}
	if p.Effort != "" {
		c["model_reasoning_effort"] = p.Effort
	}
	servers := c["mcp_servers"].(map[string]any)
	names := make([]string, 0, len(p.MCPTools))
	for n := range p.MCPTools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if n == "swarm" || len(p.MCPTools[n]) == 0 {
			continue
		}
		servers[n] = map[string]any{"enabled_tools": p.MCPTools[n]}
	}
	return encode(c)
}

// sandboxModes is the allowed set: ore 1.161 refuses a set without
// read-only ("must include 'read-only' to allow any PermissionProfile").
func sandboxModes(mode string) []string {
	if mode == "read-only" {
		return []string{mode}
	}
	return []string{"read-only", mode}
}

func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// UserSettings is what the supervisor writes into ~/.ore/config.toml.
type UserSettings struct {
	// TrustDirs are marked trusted ([projects."<dir>"] trust_level).
	TrustDirs []string
	// Provider is the gateway provider (from Policy.Provider); nil leaves it.
	Provider map[string]any
	Model    string
	Effort   string
}

// UpdateUserConfig merges s into the TOML config at path, keeping other keys.
func UpdateUserConfig(path string, s UserSettings, uid, gid int) error {
	cfg := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if _, err := toml.Decode(string(b), &cfg); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	for _, d := range s.TrustDirs {
		p, _ := projects[d].(map[string]any)
		if p == nil {
			p = map[string]any{}
		}
		p["trust_level"] = "trusted"
		projects[d] = p
	}
	cfg["projects"] = projects
	if s.Provider != nil {
		providers, _ := cfg["model_providers"].(map[string]any)
		if providers == nil {
			providers = map[string]any{}
		}
		providers["gateway"] = s.Provider
		cfg["model_providers"] = providers
		cfg["model_provider"] = "gateway"
	}
	if s.Model != "" {
		cfg["model"] = s.Model
	}
	if s.Effort != "" {
		cfg["model_reasoning_effort"] = s.Effort
	}
	b, err := encode(cfg)
	if err != nil {
		return err
	}
	tmp := path + ".swarm.tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if os.Getuid() == 0 {
		if err := os.Chown(tmp, uid, gid); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}
