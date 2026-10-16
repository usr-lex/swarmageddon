package wizard

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

func ptr[T any](v T) *T { return &v }

func TestMerge(t *testing.T) {
	ws := v1alpha1.RoomDefaults{
		Env:        map[string]string{"A": "ws", "B": "ws"},
		SecretEnv:  []v1alpha1.SecretEnvVar{{Name: "TOKEN", SecretRef: v1alpha1.SecretKeyRef{Name: "room-secrets", Key: "ws"}}},
		MCPServers: map[string]v1alpha1.MCPServer{"docs": {Command: "docs-mcp"}, "jira": {Type: "http", URL: "https://jira"}},
		Network:    &v1alpha1.NetworkAccess{Mode: v1alpha1.NetworkLimited, Domains: []string{"example.com"}, PackageManagers: ptr(true)},
	}
	role := v1alpha1.RoomDefaults{
		Env:        map[string]string{"B": "role"},
		MCPServers: map[string]v1alpha1.MCPServer{"jira": {Disabled: true}},
		Network:    &v1alpha1.NetworkAccess{Domains: []string{"*.github.com"}},
	}
	room := v1alpha1.RoomDefaults{
		SecretEnv: []v1alpha1.SecretEnvVar{{Name: "TOKEN", SecretRef: v1alpha1.SecretKeyRef{Name: "room-secrets", Key: "mine"}}},
		Network:   &v1alpha1.NetworkAccess{PackageManagers: ptr(false), Domains: []string{"example.com"}},
	}
	e := Effective("athena", "implementer", ws, role, room)
	s := e.Settings
	if s.Env["A"] != "ws" || s.Env["B"] != "role" {
		t.Fatalf("env %v", s.Env)
	}
	if len(s.SecretEnv) != 1 || s.SecretEnv[0].Secret.Key != "mine" {
		t.Fatalf("secretEnv %v", s.SecretEnv)
	}
	if _, ok := s.McpServers["jira"]; ok || s.McpServers["docs"] == nil {
		t.Fatalf("mcp %v", s.McpServers)
	}
	n := s.Network
	if n.Mode != "limited" || strings.Join(n.Domains, ",") != "example.com,*.github.com" || n.GetPackageManagers() {
		t.Fatalf("network %+v", n)
	}
	src := map[string]string{}
	for _, x := range e.Sources {
		src[x.Kind+"/"+x.Key] = x.Level
	}
	want := map[string]string{"env/A": "workspace", "env/B": "role", "secretEnv/TOKEN": "room", "mcpServer/docs": "workspace",
		"network/mode": "workspace", "domain/*.github.com": "role", "domain/example.com": "workspace", "packageManagers/": "room"}
	for k, v := range want {
		if src[k] != v {
			t.Errorf("source %s = %q, want %q", k, src[k], v)
		}
	}
	if _, ok := src["mcpServer/jira"]; ok {
		t.Error("disabled server still has a source")
	}
	// Nothing set anywhere: full network.
	if e := Effective("x", "y", v1alpha1.RoomDefaults{}, v1alpha1.RoomDefaults{}, v1alpha1.RoomDefaults{}); e.Settings.Network.Mode != "full" {
		t.Fatalf("default network %+v", e.Settings.Network)
	}
	// Deprecated internet: false is network none.
	w := &v1alpha1.Workspace{Spec: v1alpha1.WorkspaceSpec{Internet: ptr(false)}} //nolint:staticcheck // an old workspace
	if WorkspaceDefaults(w).Network.Mode != v1alpha1.NetworkNone {
		t.Fatal("internet false not mapped to none")
	}
}

func TestPlanConversionRoundTrip(t *testing.T) {
	d := v1alpha1.RoomDefaults{
		Env:       map[string]string{"A": "1"},
		SecretEnv: []v1alpha1.SecretEnvVar{{Name: "T", SecretRef: v1alpha1.SecretKeyRef{Name: "s", Key: "k"}}},
		MCPServers: map[string]v1alpha1.MCPServer{"x": {Type: "http", URL: "https://x",
			Headers: map[string]v1alpha1.MCPValue{"Authorization": {ValueFrom: &v1alpha1.SecretKeyRef{Name: "s", Key: "h"}}, "X-Team": {Value: "a"}}}},
		Network: &v1alpha1.NetworkAccess{Mode: v1alpha1.NetworkLimited, Domains: []string{"a.com"}, PackageManagers: ptr(true)},
	}
	got := SettingsFromPlan(SettingsToPlan(d))
	if specJSON(&got) != specJSON(&d) {
		t.Fatalf("round trip\n%s\n%s", specJSON(&got), specJSON(&d))
	}
}

func TestValidateSettingsAndVolumes(t *testing.T) {
	p := plan()
	p.RoomDefaults = &ccv1.RoomSettingsPlan{
		Env:       map[string]string{"1BAD": "x", "OPENAI": "sk-abcdefghijklmnop"},
		SecretEnv: []*ccv1.SecretEnvPlan{{Name: "TOKEN"}},
		McpServers: map[string]*ccv1.McpServerPlan{
			"swarm": {Command: "x"},
			"web":   {Type: "http"},
			"cmd":   {Env: map[string]*ccv1.McpValuePlan{"K": {Value: "v", ValueFrom: &ccv1.SecretKeyPlan{Name: "s", Key: "k"}}}},
		},
		Network: &ccv1.NetworkPlan{Mode: "some", Domains: []string{"https://x.com"}},
	}
	p.SharedVolumes = []*ccv1.SharedVolumePlan{
		{Name: "data", Size: "10Gi", Mounts: []*ccv1.SharedVolumeMountPlan{{Select: "everyone"}}},
		{Name: "Bad", Size: "lots", Mounts: []*ccv1.SharedVolumeMountPlan{{Select: "team:x"}}},
		{Name: "ok", Size: "1Gi", Mounts: []*ccv1.SharedVolumeMountPlan{{Select: "group:clean"}, {Select: "group:dirty", ReadOnly: true}}},
	}
	p.Agents = append(p.Agents, &ccv1.AgentPlan{Name: "metis", Role: "implementer", Cli: "claude", Group: "dirty"})
	joined := strings.Join(Validate(p, map[string]bool{}), "\n")
	for _, want := range []string{`"1BAD" is not a valid name`, `"OPENAI" looks like a secret`, `"TOKEN" needs a Secret`, `"swarm" is reserved`,
		`"web" needs a URL`, `"cmd" needs a command`, "both a value and a Secret", "network mode", `"https://x.com"`,
		`"data" is read-write on both sides of the wall between clean and dirty`, `"Bad": lower case`, `size "lots"`, `selector "team:x"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, `"ok"`) {
		t.Errorf("read-only across the wall refused:\n%s", joined)
	}
	vols := RoomVolumes(volumesFromPlan(p), "metis", "implementer", "dirty")
	if vols["data"] != "rw" || vols["ok"] != "ro" {
		t.Fatalf("RoomVolumes %v", vols)
	}
}

func TestApplyLayers(t *testing.T) {
	ctx := context.Background()
	ws := &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Status: v1alpha1.WorkspaceStatus{Namespace: "ws-acme"}}
	impl := &v1alpha1.RoleTemplate{ObjectMeta: metav1.ObjectMeta{Name: "implementer", Namespace: "ws-acme"}, Spec: v1alpha1.RoleTemplateSpec{Title: "Implementer"}}
	c := newClient(ws, impl)
	p := plan()
	p.RoomDefaults = &ccv1.RoomSettingsPlan{Env: map[string]string{"LANG": "C"}, Network: &ccv1.NetworkPlan{Mode: "limited", Domains: []string{"example.com"}}}
	p.RoleDefaults = map[string]*ccv1.RoomSettingsPlan{"implementer": {Env: map[string]string{"LANG": "en"}}}
	p.Agents[1].Settings = &ccv1.RoomSettingsPlan{SecretEnv: []*ccv1.SecretEnvPlan{{Name: "GH", Secret: &ccv1.SecretKeyPlan{Name: "room-secrets", Key: "gh"}}}}
	p.SharedVolumes = []*ccv1.SharedVolumePlan{{Name: "data", Size: "10Gi", Mounts: []*ccv1.SharedVolumeMountPlan{{Select: "group:clean"}}}}
	if probs := Validate(p, map[string]bool{}); len(probs) != 0 {
		t.Fatal(probs)
	}
	cur, _ := LoadCurrent(ctx, c, "acme")
	d := Build(p, cur)
	if d.Workspace.Spec.Internet != nil { //nolint:staticcheck // must be cleared
		t.Fatal("internet kept next to a network setting")
	}
	changes := Diff(cur, d, false)
	if err := Apply(ctx, c, cur, d, changes, time.Second); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.RoleTemplate
	c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "implementer"}, &got)
	if got.Spec.RoomDefaults.Env["LANG"] != "en" || got.Spec.Title != "Implementer" {
		t.Fatalf("role template %+v", got.Spec)
	}
	var hermes v1alpha1.Room
	c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "hermes"}, &hermes)
	if len(hermes.Spec.SecretEnv) != 1 {
		t.Fatalf("room secretEnv %+v", hermes.Spec)
	}
	cur, _ = LoadCurrent(ctx, c, "acme")
	eff := EffectiveCurrent(cur)
	if len(eff) != 2 || eff[1].Room != "hermes" || eff[1].Settings.Env["LANG"] != "en" || eff[0].Settings.Env["LANG"] != "C" ||
		eff[1].Settings.Network.Mode != "limited" || len(eff[1].Settings.SecretEnv) != 1 {
		t.Fatalf("effective %+v", eff)
	}
	// Read back and re-apply: nothing changes.
	back := FromCluster(cur)
	if back.RoleDefaults["implementer"] == nil || len(back.SharedVolumes) != 1 || back.Agents[1].Settings == nil {
		t.Fatalf("FromCluster %+v", back)
	}
	for _, ch := range Diff(cur, Build(back, cur), false) {
		if ch.Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			t.Fatalf("round trip changes %s/%s %v\n%s\n%s", ch.Kind, ch.Name, ch.Fields, ch.Before, ch.After)
		}
	}
	// Defaults for a custom template that doesn't exist are refused.
	p.RoleDefaults["ghost"] = &ccv1.RoomSettingsPlan{}
	if m := MissingTemplates(Build(p, cur)); len(m) != 1 {
		t.Fatalf("missing templates %v", m)
	}
}
