package persona

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/BurntSushi/toml"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite golden files")

func fixture(cli v1.CLI) Input {
	ws := &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1.WorkspaceSpec{
		Forge:  v1.ForgeSpec{Org: "acme"},
		Models: v1.ModelSettings{Default: "claude-opus-5-5"},
	}}
	role := &v1.RoleTemplate{ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: "ws-acme"}, Spec: v1.RoleTemplateSpec{
		Title: "Reviewer", Summary: "Reviews every pull request against the task card.",
		RoleText: "You review pull requests.\n",
		ClaudeCode: v1.ClaudeCodePolicy{
			PermissionMode: "acceptEdits", Allow: []string{"Bash(npm test:*)"}, Deny: []string{"Bash(git push --force:*)"},
			Hooks: &runtime.RawExtension{Raw: []byte(`{"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"lint-hook"}]}]}`)},
		},
		Ore: v1.OrePolicy{
			Rules:    "prefix_rule(pattern=[\"git\", \"push\", \"--force\"], decision=\"forbidden\")\n",
			MCPTools: map[string][]string{"swarm": {"send_message", "list_agents"}},
		},
	}}
	room := &v1.Room{ObjectMeta: metav1.ObjectMeta{Name: "athena", Namespace: "ws-acme"}, Spec: v1.RoomSpec{
		DisplayName: "Athena", Role: "reviewer", CLI: cli, Effort: "high", Group: "clean",
		Repos: []string{"acme/api"}, Env: map[string]string{"FOO": "bar"},
	}}
	return Input{Room: room, Role: role, Workspace: ws, Endpoints: Endpoints{
		ForgeURL:     "http://forgejo.swarmageddon-platform.svc:3000",
		ForgeSSHHost: "forgejo-ssh.swarmageddon-platform.svc",
		MessagingURL: "https://messaging.swarmageddon-platform.svc:8443",
		GatewayURL:   "http://gateway.swarmageddon-platform.svc:4000",
	}}
}

func golden(t *testing.T, name string, data map[string]string) {
	t.Helper()
	dir := filepath.Join("testdata", name)
	if *update {
		_ = os.RemoveAll(dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range data {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	var want []string
	for _, e := range entries {
		want = append(want, e.Name())
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if data[e.Name()] != string(b) {
			t.Errorf("%s/%s differs from golden:\n%s", name, e.Name(), data[e.Name()])
		}
	}
	var got []string
	for k := range data {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Errorf("keys = %v, golden has %v", got, want)
	}
}

func TestClaudeGolden(t *testing.T) {
	data, err := Render(fixture(v1.CLIClaude))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "claude", data)

	var s map[string]any
	if err := json.Unmarshal([]byte(data[KeyClaudeManagedSettings]), &s); err != nil {
		t.Fatal(err)
	}
	if s["crossSessionInbound"] != "accept" || s["allowManagedHooksOnly"] != true {
		t.Errorf("settings = %v", s)
	}
	hooks := s["hooks"].(map[string]any)
	post := hooks["PostToolUse"].([]any)
	if len(post) != 2 || post[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "swarm hook PostToolUse" {
		t.Errorf("our hook must come first, then the template's: %v", post)
	}
	pr := hooks["PermissionRequest"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if pr["timeout"] != float64(86400) {
		t.Errorf("PermissionRequest timeout = %v", pr["timeout"])
	}
	if s["env"].(map[string]any)["ANTHROPIC_BASE_URL"] != "http://gateway.swarmageddon-platform.svc:4000" {
		t.Error("ANTHROPIC_BASE_URL")
	}
	if _, ok := data[KeyOreConfig]; ok {
		t.Error("ore files in a Claude room")
	}
}

func TestOreGolden(t *testing.T) {
	data, err := Render(fixture(v1.CLIOre))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "ore", data)

	var req map[string]any
	if _, err := toml.Decode(data[KeyOreRequirements], &req); err != nil {
		t.Fatal(err)
	}
	gw := req["model_providers"].(map[string]any)["gateway"].(map[string]any)
	if gw["env_key"] != "SWARM_GATEWAY_KEY" || gw["base_url"] != "http://gateway.swarmageddon-platform.svc:4000/v1" || gw["wire_api"] != "anthropic" {
		t.Errorf("gateway provider = %v", gw)
	}
	var cfg map[string]any
	if _, err := toml.Decode(data[KeyOreConfig], &cfg); err != nil {
		t.Fatal(err)
	}
	sw := cfg["mcp_servers"].(map[string]any)["swarm"].(map[string]any)
	if sw["command"] != "swarm" || len(sw["enabled_tools"].([]any)) != 2 {
		t.Errorf("swarm mcp = %v", sw)
	}
	if cfg["approval_policy"] != "on-request" || cfg["approvals_reviewer"] != "user" {
		t.Errorf("cfg = %v", cfg)
	}
	if data[KeyOreRules] == "" {
		t.Error("rules missing")
	}
}

func TestOreBypass(t *testing.T) {
	in := fixture(v1.CLIOre)
	in.Role.Spec.Ore.Bypass = true
	data, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_, _ = toml.Decode(data[KeyOreConfig], &cfg)
	if cfg["approval_policy"] != "never" || cfg["sandbox_mode"] != "danger-full-access" {
		t.Errorf("cfg = %v", cfg)
	}
}

func TestRoomJSONDefaults(t *testing.T) {
	in := fixture(v1.CLIClaude)
	in.Room.Spec.Model = ""
	in.Room.Spec.WorkDir = ""
	data, _ := Render(in)
	var rj RoomJSON
	if err := json.Unmarshal([]byte(data[KeyRoomJSON]), &rj); err != nil {
		t.Fatal(err)
	}
	if rj.Model != "claude-opus-5-5" || rj.WorkDir != "work" || !rj.Sudo || rj.Email != "athena@agents.acme.invalid" || rj.Address != "athena@acme" {
		t.Errorf("room.json = %+v", rj)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	a, _ := Render(fixture(v1.CLIClaude))
	for range 20 {
		b, _ := Render(fixture(v1.CLIClaude))
		for k := range a {
			if a[k] != b[k] {
				t.Fatalf("%s not deterministic", k)
			}
		}
	}
}
