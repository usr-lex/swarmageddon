package persona

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"k8s.io/utils/ptr"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestMergeFor(t *testing.T) {
	ws := &v1.Workspace{Spec: v1.WorkspaceSpec{RoomDefaults: v1.RoomDefaults{
		Env: map[string]string{"A": "ws", "SWARM_ROOM": "evil", "TOKEN": "plain"},
	}}}
	role := &v1.RoleTemplate{Spec: v1.RoleTemplateSpec{RoomDefaults: v1.RoomDefaults{
		SecretEnv: []v1.SecretEnvVar{{Name: "TOKEN", SecretRef: v1.SecretKeyRef{Name: "room-secrets", Key: "t"}}, {Name: "HTTPS_PROXY", SecretRef: v1.SecretKeyRef{Name: "s", Key: "k"}}},
	}}}
	room := &v1.Room{Spec: v1.RoomSpec{Env: map[string]string{"C": "room"}}}
	got := MergeFor(ws, role, room)
	if !reflect.DeepEqual(got.Env, map[string]string{"A": "ws", "C": "room"}) || len(got.SecretEnv) != 1 || got.SecretEnv[0].Name != "TOKEN" {
		t.Errorf("merged = %+v", got)
	}
	if NetworkMode(got) != v1.NetworkFull {
		t.Error("default mode is not full")
	}
}

func TestResolve(t *testing.T) {
	d := v1.RoomDefaults{
		SecretEnv: []v1.SecretEnvVar{{Name: "TOKEN", SecretRef: v1.SecretKeyRef{Name: "s", Key: "k"}}, {Name: "GONE", SecretRef: v1.SecretKeyRef{Name: "missing", Key: "k"}}},
		MCPServers: map[string]v1.MCPServer{"docs": {URL: "https://docs.example/mcp", Headers: map[string]v1.MCPValue{
			"Authorization": {ValueFrom: &v1.SecretKeyRef{Name: "s", Key: "auth"}}, "X-Team": {Value: "acme"},
		}}},
	}
	get := func(r v1.SecretKeyRef) (string, error) {
		if r.Name == "missing" {
			return "", errors.New("not found")
		}
		return "val-" + r.Key, nil
	}
	s, values, errs := Resolve(d, get)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "GONE") {
		t.Errorf("errs = %v", errs)
	}
	if !reflect.DeepEqual(s.SecretEnv, []string{"TOKEN"}) || values["TOKEN"] != "val-k" {
		t.Errorf("secret env = %v %v", s.SecretEnv, values)
	}
	docs := s.MCPServers["docs"]
	if docs.Type != "http" || docs.Headers["Authorization"] != "val-auth" || docs.Headers["X-Team"] != "acme" {
		t.Errorf("docs = %+v", docs)
	}
}

func layered(cli v1.CLI) Input {
	in := fixture(cli)
	in.Role.Spec.Advisor = &v1.AdvisorSpec{Model: "claude-fable-5", Native: true}
	in.Role.Spec.WardenRules = []v1.WardenRule{{Name: "no-prod", Tool: "Bash", Match: "prod", Action: v1.WardenDeny}}
	in.Workspace.Spec.Warden = v1.WardenSpec{Mode: v1.WardenGate, Model: "small"}
	d := v1.RoomDefaults{
		Env: map[string]string{"FOO": "bar"},
		MCPServers: map[string]v1.MCPServer{
			"docs": {URL: "https://docs.example/mcp", Headers: map[string]v1.MCPValue{"Authorization": {Value: "Bearer x"}}},
			"jira": {Command: "jira-mcp", Args: []string{"--ro"}, Env: map[string]v1.MCPValue{"JIRA_TOKEN": {Value: "t"}}, EnabledTools: []string{"search"}},
		},
		Network: &v1.NetworkAccess{Mode: v1.NetworkLimited, Domains: []string{"api.example.com"}, PackageManagers: ptr.To(true)},
	}
	s, _, _ := Resolve(d, nil)
	s.Network = NetworkFor(d, "http://egress:3128", []string{".svc"}, []string{"pypi.org"})
	in.Settings = &s
	return in
}

func TestLayeredClaude(t *testing.T) {
	data, err := Render(layered(v1.CLIClaude))
	if err != nil {
		t.Fatal(err)
	}
	var mcp struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(data[KeyClaudeManagedMCP]), &mcp); err != nil {
		t.Fatal(err)
	}
	if mcp.MCPServers["swarm"]["command"] != "swarm" || mcp.MCPServers["docs"]["type"] != "http" ||
		mcp.MCPServers["docs"]["headers"].(map[string]any)["Authorization"] != "Bearer x" || mcp.MCPServers["jira"]["env"].(map[string]any)["JIRA_TOKEN"] != "t" {
		t.Errorf("managed-mcp = %v", mcp.MCPServers)
	}
	agent := data[KeyClaudeAgentAdvisor]
	if !strings.Contains(agent, "tools: Read, Grep, Glob") || !strings.Contains(agent, "model: claude-fable-5") || !strings.Contains(agent, "effort: high") {
		t.Errorf("advisor agent = %s", agent)
	}
	var s map[string]any
	_ = json.Unmarshal([]byte(data[KeyClaudeManagedSettings]), &s)
	if s["advisorModel"] != "claude-fable-5" || s["env"].(map[string]any)["CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL"] != "1" {
		t.Errorf("native advisor settings = %v", s)
	}
	var rj RoomJSON
	_ = json.Unmarshal([]byte(data[KeyRoomJSON]), &rj)
	if rj.Advisor == nil || rj.Advisor.MaxCallsPerHour != 10 || !rj.Advisor.Native || rj.Warden.Mode != "gate" || rj.Warden.Model != "small" ||
		len(rj.Warden.Rules) != 1 || !rj.Warden.InterruptOnHigh {
		t.Errorf("room.json advisor/warden = %+v %+v", rj.Advisor, rj.Warden)
	}
	if rj.Network.Mode != "limited" || rj.Network.ProxyURL != "http://egress:3128" || !reflect.DeepEqual(rj.Network.Domains, []string{"api.example.com", "pypi.org"}) ||
		!reflect.DeepEqual(rj.MCPServers, []string{"docs", "jira"}) {
		t.Errorf("room.json = %+v", rj)
	}

	// Not native: no flag, no variable; the subagent stays.
	in := layered(v1.CLIClaude)
	in.Role.Spec.Advisor.Native = false
	data, _ = Render(in)
	if strings.Contains(data[KeyClaudeManagedSettings], "ADVISOR") || strings.Contains(data[KeyClaudeManagedSettings], "advisorModel") || data[KeyClaudeAgentAdvisor] == "" {
		t.Error("non-native advisor")
	}
	// No advisor: no subagent.
	in.Role.Spec.Advisor = nil
	data, _ = Render(in)
	if _, ok := data[KeyClaudeAgentAdvisor]; ok {
		t.Error("advisor agent without an advisor")
	}
}

func TestLayeredOre(t *testing.T) {
	data, err := Render(layered(v1.CLIOre))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(data[KeyOreConfig], &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers["docs"]["url"] != "https://docs.example/mcp" || cfg.MCPServers["docs"]["http_headers"].(map[string]any)["Authorization"] != "Bearer x" {
		t.Errorf("docs = %v", cfg.MCPServers["docs"])
	}
	if cfg.MCPServers["jira"]["command"] != "jira-mcp" || cfg.MCPServers["jira"]["env"].(map[string]any)["JIRA_TOKEN"] != "t" || len(cfg.MCPServers["jira"]["enabled_tools"].([]any)) != 1 {
		t.Errorf("jira = %v", cfg.MCPServers["jira"])
	}
	var req struct {
		MCPServers map[string]struct {
			Identity map[string]string `toml:"identity"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.Decode(data[KeyOreRequirements], &req); err != nil {
		t.Fatal(err)
	}
	if req.MCPServers["docs"].Identity["url"] != "https://docs.example/mcp" || req.MCPServers["jira"].Identity["command"] != "jira-mcp" || req.MCPServers["swarm"].Identity["command"] != "swarm" {
		t.Errorf("requirements = %+v", req.MCPServers)
	}
}
