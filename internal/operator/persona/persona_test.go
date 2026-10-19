package persona

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/ore"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
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
	// The room's own tools never ask (OpenShift report, issue 3): ore asked
	// for every swarm call and "always allow" couldn't persist. The canary
	// runs on ore.Config; the setting must match what ships.
	canary, err := ore.Config(ore.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	var cc map[string]any
	if _, err := toml.Decode(string(canary), &cc); err != nil {
		t.Fatal(err)
	}
	want := cc["mcp_servers"].(map[string]any)["swarm"].(map[string]any)["default_tools_approval_mode"]
	if want != "approve" || sw["default_tools_approval_mode"] != want {
		t.Errorf("swarm default_tools_approval_mode = %v, canary's %v", sw["default_tools_approval_mode"], want)
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
	// ore refuses to start unless read-only is in the allowed set.
	var req map[string]any
	_, _ = toml.Decode(data[KeyOreRequirements], &req)
	if got := fmt.Sprint(req["allowed_sandbox_modes"]); got != "[read-only danger-full-access]" {
		t.Errorf("allowed_sandbox_modes = %s", got)
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

// TestRoomJSONTunables: storm, janitor and the warden's review cadence reach
// the supervisor's room.json, and stay out of it when unset.
func TestRoomJSONTunables(t *testing.T) {
	in := fixture(v1.CLIClaude)
	data, _ := Render(in)
	for _, k := range []string{`"storm"`, `"janitor"`, `"reviewEveryToolCalls"`} {
		if strings.Contains(data[KeyRoomJSON], k) {
			t.Errorf("unset %s rendered:\n%s", k, data[KeyRoomJSON])
		}
	}

	in.Workspace.Spec.Messaging.Storm.CoalesceBytes = 2048
	in.Workspace.Spec.Warden.ReviewEveryToolCalls = 7
	in.Room.Spec.Janitor = &v1.JanitorSpec{IntervalMinutes: 30, PruneAfterHours: 12, ImagesAbovePercent: 60, WarnPercent: 75, AggressivePercent: 85}
	data, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "room.json")
	if err := os.WriteFile(f, []byte(data[KeyRoomJSON]), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := roomcfg.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	want := roomcfg.JanitorSettings{IntervalMinutes: 30, PruneAfterHours: 12, ImagesAbovePercent: 60, WarnPercent: 75, AggressivePercent: 85}
	if r.Janitor != want {
		t.Errorf("janitor = %+v, want %+v", r.Janitor, want)
	}
	if r.Storm.CoalesceLimit() != 2048 || r.Warden.ReviewEveryToolCalls != 7 {
		t.Errorf("storm %+v, warden %+v", r.Storm, r.Warden)
	}

	in.Room.Spec.Janitor = &v1.JanitorSpec{Disabled: true}
	data, _ = Render(in)
	var rj RoomJSON
	if err := json.Unmarshal([]byte(data[KeyRoomJSON]), &rj); err != nil {
		t.Fatal(err)
	}
	if !rj.Janitor.Disabled {
		t.Error("janitor not disabled")
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

// Direct model access (OpenShift report, feature 8): the CLI talks to its
// provider itself. Claude Code gets no gateway base URL, ore no forced
// gateway provider, and room.json says so, so the supervisor issues no key.
func TestDirectModelAccess(t *testing.T) {
	for _, cli := range []v1.CLI{v1.CLIClaude, v1.CLIOre} {
		in := fixture(cli)
		in.Workspace.Spec.Models.Access = v1.ModelAccessDirect
		data, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		var rj RoomJSON
		if err := json.Unmarshal([]byte(data[KeyRoomJSON]), &rj); err != nil {
			t.Fatal(err)
		}
		if rj.ModelAccess != "direct" || rj.GatewayURL == "" {
			t.Errorf("%s room.json: access %q gateway %q", cli, rj.ModelAccess, rj.GatewayURL)
		}
		switch cli {
		case v1.CLIClaude:
			if strings.Contains(data[KeyClaudeManagedSettings], "ANTHROPIC_BASE_URL") {
				t.Error("claude: managed settings still point at the gateway")
			}
		case v1.CLIOre:
			for _, k := range []string{KeyOreRequirements, KeyOreConfig} {
				var m map[string]any
				if _, err := toml.Decode(data[k], &m); err != nil {
					t.Fatal(err)
				}
				if _, ok := m["model_provider"]; ok {
					t.Errorf("ore %s still forces a provider: %v", k, m["model_provider"])
				}
				if _, ok := m["model_providers"]; ok {
					t.Errorf("ore %s still defines the gateway provider", k)
				}
			}
		}
		// A room may go back to the gateway.
		in.Room.Spec.ModelAccess = v1.ModelAccessGateway
		data, _ = Render(in)
		if strings.Contains(data[KeyRoomJSON], `"modelAccess"`) {
			t.Errorf("%s: room override to gateway ignored", cli)
		}
	}
}

func TestDirectAccessNetwork(t *testing.T) {
	none := v1.RoomDefaults{Network: &v1.NetworkAccess{Mode: v1.NetworkNone}}
	got := DirectAccessNetwork(none, v1.CLIClaude)
	if got.Network.Mode != v1.NetworkLimited || !slices.Contains(got.Network.Domains, "api.anthropic.com") || len(got.Network.Domains) != len(ProviderDomains(v1.CLIClaude)) {
		t.Errorf("none: %+v", got.Network)
	}
	limited := v1.RoomDefaults{Network: &v1.NetworkAccess{Mode: v1.NetworkLimited, Domains: []string{"example.com"}}}
	got = DirectAccessNetwork(limited, v1.CLIOre)
	if got.Network.Mode != v1.NetworkLimited || !slices.Contains(got.Network.Domains, "example.com") || !slices.Contains(got.Network.Domains, "api.openai.com") {
		t.Errorf("limited: %+v", got.Network)
	}
	if limited.Network.Domains[0] != "example.com" || len(limited.Network.Domains) != 1 {
		t.Error("input modified")
	}
	if got := DirectAccessNetwork(v1.RoomDefaults{}, v1.CLIClaude); got.Network != nil && got.Network.Mode != v1.NetworkFull && got.Network.Mode != "" {
		t.Errorf("full: %+v", got.Network)
	}
}
