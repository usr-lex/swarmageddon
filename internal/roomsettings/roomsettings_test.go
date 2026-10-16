package roomsettings

import (
	"reflect"
	"testing"

	"k8s.io/utils/ptr"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestMerge(t *testing.T) {
	ws := v1.RoomDefaults{
		Env:        map[string]string{"A": "ws", "B": "ws"},
		SecretEnv:  []v1.SecretEnvVar{{Name: "TOKEN", SecretRef: v1.SecretKeyRef{Name: "room-secrets", Key: "ws"}}},
		MCPServers: map[string]v1.MCPServer{"docs": {Type: "http", URL: "u"}, "legacy": {Command: "old"}},
		Network:    &v1.NetworkAccess{Mode: v1.NetworkLimited, Domains: []string{"a.example"}, PackageManagers: ptr.To(true)},
	}
	role := v1.RoomDefaults{
		Env:        map[string]string{"B": "role"},
		MCPServers: map[string]v1.MCPServer{"legacy": {Disabled: true}},
		Network:    &v1.NetworkAccess{Domains: []string{"b.example", "a.example"}},
	}
	room := v1.RoomDefaults{
		SecretEnv: []v1.SecretEnvVar{{Name: "TOKEN", SecretRef: v1.SecretKeyRef{Name: "room-secrets", Key: "room"}}},
		Network:   &v1.NetworkAccess{PackageManagers: ptr.To(false)},
	}
	m := Merge(ws, role, room)
	s := m.Settings
	if !reflect.DeepEqual(s.Env, map[string]string{"A": "ws", "B": "role"}) || s.SecretEnv[0].SecretRef.Key != "room" {
		t.Errorf("env = %v %v", s.Env, s.SecretEnv)
	}
	if _, ok := s.MCPServers["legacy"]; ok || len(s.MCPServers) != 1 {
		t.Errorf("mcp = %v", s.MCPServers)
	}
	if s.Network.Mode != v1.NetworkLimited || !reflect.DeepEqual(s.Network.Domains, []string{"a.example", "b.example"}) || *s.Network.PackageManagers {
		t.Errorf("network = %+v", s.Network)
	}
	want := map[Source]bool{
		{"env", "B", LevelRole}: true, {"secretEnv", "TOKEN", LevelRoom}: true, {"network", "mode", LevelWorkspace}: true,
		{"domain", "b.example", LevelRole}: true, {"packageManagers", "", LevelRoom}: true,
	}
	for _, src := range m.Sources {
		delete(want, src)
		if src.Kind == "mcpServer" && src.Key == "legacy" {
			t.Error("disabled server has a source")
		}
	}
	if len(want) > 0 {
		t.Errorf("missing sources %v in %v", want, m.Sources)
	}
	if d := Merge(v1.RoomDefaults{}, v1.RoomDefaults{}, v1.RoomDefaults{}); d.Settings.Network.Mode != v1.NetworkFull || d.Sources[0].Level != LevelDefault {
		t.Errorf("default = %+v", d)
	}
	off := &v1.Workspace{Spec: v1.WorkspaceSpec{Internet: ptr.To(false)}}
	if WorkspaceLevel(off).Network.Mode != v1.NetworkNone {
		t.Error("internet: false")
	}
}

func TestWalled(t *testing.T) {
	links := []v1.GroupLink{{From: "clean", To: "dirty", Mode: v1.LinkMediated}, {From: "a", To: "b", Mode: v1.LinkOpen}}
	if !Walled(links, "dirty", "clean") || Walled(links, "a", "b") || Walled(links, "a", "a") {
		t.Error("walls")
	}
	if !Selects("group:clean", "athena", "reviewer", "clean") || Selects("group:", "athena", "reviewer", "") || !Selects("role:reviewer", "x", "reviewer", "") {
		t.Error("selects")
	}
}
