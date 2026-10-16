package ore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestRequirementsAndConfig(t *testing.T) {
	p := Policy{GatewayURL: "http://gw:4000/v1", Model: "gpt-x", Effort: "high",
		MCPTools: map[string][]string{"swarm": {"send_message", "list_agents"}}}
	b, err := Requirements(p)
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if _, err := toml.Decode(string(b), &r); err != nil {
		t.Fatal(err)
	}
	if r["allowed_approval_policies"].([]any)[0] != "on-request" || r["allowed_approvals_reviewers"].([]any)[0] != "user" || r["model_provider"] != "gateway" {
		t.Errorf("requirements:\n%s", b)
	}
	gw := r["model_providers"].(map[string]any)["gateway"].(map[string]any)
	if gw["env_key"] != GatewayKeyEnv || gw["wire_api"] != "responses" || gw["base_url"] != "http://gw:4000/v1" {
		t.Errorf("provider %v", gw)
	}
	if !strings.Contains(string(b), "[mcp_servers.swarm.identity]") {
		t.Errorf("no swarm identity:\n%s", b)
	}
	c, err := Config(p)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if _, err := toml.Decode(string(c), &cfg); err != nil {
		t.Fatal(err)
	}
	swarm := cfg["mcp_servers"].(map[string]any)["swarm"].(map[string]any)
	if swarm["command"] != "swarm" || len(swarm["enabled_tools"].([]any)) != 2 || cfg["model"] != "gpt-x" || cfg["sandbox_mode"] != "workspace-write" {
		t.Errorf("config:\n%s", c)
	}
	b, _ = Requirements(Policy{Bypass: true})
	if !strings.Contains(string(b), `"never"`) || !strings.Contains(string(b), `"danger-full-access"`) {
		t.Errorf("bypass:\n%s", b)
	}
}

func TestUpdateUserConfig(t *testing.T) {
	f := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(f, []byte("mine = 1\n[projects.\"/old\"]\ntrust_level = \"trusted\"\n"), 0o600)
	err := UpdateUserConfig(f, UserSettings{TrustDirs: []string{"/home/a/work"}, Provider: Policy{GatewayURL: "http://gw"}.Provider(), Model: "m"}, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	b, _ := os.ReadFile(f)
	if _, err := toml.Decode(string(b), &cfg); err != nil {
		t.Fatal(err)
	}
	pr := cfg["projects"].(map[string]any)
	if cfg["mine"] != int64(1) || pr["/old"] == nil || pr["/home/a/work"].(map[string]any)["trust_level"] != "trusted" || cfg["model_provider"] != "gateway" || cfg["model"] != "m" {
		t.Errorf("config:\n%s", b)
	}
}

func TestIsNoActiveTurn(t *testing.T) {
	if !isNoActiveTurn(&RPCError{Code: -32600, Message: "no active turn to steer"}) {
		t.Error("no active turn")
	}
	if isNoActiveTurn(errors.New("no active turn")) || isNoActiveTurn(&RPCError{Message: "thread not found"}) {
		t.Error("false positive")
	}
}
