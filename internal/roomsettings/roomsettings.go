// Package roomsettings merges layered room settings (docs/design.md §6.6):
// the workspace's roomDefaults, then the role template's, then the room's
// own spec, later levels overriding earlier ones by key. It is the single
// definition both the operator (which renders the result into rooms) and the
// control center (which shows it) use. It does not touch Kubernetes.
package roomsettings

import (
	"maps"
	"slices"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Levels, in merge order. LevelDefault marks a value no level set.
const (
	LevelWorkspace = "workspace"
	LevelRole      = "role"
	LevelRoom      = "room"
	LevelDefault   = "default"
)

// Source says which level an entry of the merged settings came from.
type Source struct {
	// Kind is env, secretEnv, mcpServer, network, domain or packageManagers.
	Kind string
	// Key is the variable, server or domain name; "mode" for the network
	// mode; empty for packageManagers.
	Key   string
	Level string
}

// Merged is the result of a merge.
type Merged struct {
	// Settings has no disabled MCP servers, and always a Network with a mode
	// (full when no level sets one). Domains keep first-seen order.
	Settings v1alpha1.RoomDefaults
	// Sources, sorted by kind and key.
	Sources []Source
}

// Merge merges the three levels: env and secretEnv by variable name, MCP
// servers by name (disabled removes an inherited server), the network mode
// as a whole from the latest level that sets it, domains as a union, and
// packageManagers from the latest level that sets it.
func Merge(ws, role, room v1alpha1.RoomDefaults) Merged {
	type key struct{ kind, key string }
	src := map[key]string{}
	env := map[string]string{}
	senv := map[string]v1alpha1.SecretEnvVar{}
	mcp := map[string]v1alpha1.MCPServer{}
	net := v1alpha1.NetworkAccess{Mode: v1alpha1.NetworkFull}
	src[key{"network", "mode"}] = LevelDefault
	seen := map[string]bool{}
	for i, d := range []v1alpha1.RoomDefaults{ws, role, room} {
		lvl := []string{LevelWorkspace, LevelRole, LevelRoom}[i]
		for k, v := range d.Env {
			env[k] = v
			src[key{"env", k}] = lvl
		}
		for _, e := range d.SecretEnv {
			senv[e.Name] = e
			src[key{"secretEnv", e.Name}] = lvl
		}
		for k, v := range d.MCPServers {
			if v.Disabled {
				delete(mcp, k)
				delete(src, key{"mcpServer", k})
				continue
			}
			mcp[k] = *v.DeepCopy()
			src[key{"mcpServer", k}] = lvl
		}
		if n := d.Network; n != nil {
			if n.Mode != "" {
				net.Mode = n.Mode
				src[key{"network", "mode"}] = lvl
			}
			for _, dom := range n.Domains {
				if !seen[dom] {
					seen[dom] = true
					net.Domains = append(net.Domains, dom)
					src[key{"domain", dom}] = lvl
				}
			}
			if n.PackageManagers != nil {
				pm := *n.PackageManagers
				net.PackageManagers = &pm
				src[key{"packageManagers", ""}] = lvl
			}
		}
	}
	var m Merged
	if len(env) > 0 {
		m.Settings.Env = env
	}
	for _, k := range slices.Sorted(maps.Keys(senv)) {
		m.Settings.SecretEnv = append(m.Settings.SecretEnv, senv[k])
	}
	if len(mcp) > 0 {
		m.Settings.MCPServers = mcp
	}
	m.Settings.Network = &net
	for k, lvl := range src {
		m.Sources = append(m.Sources, Source{Kind: k.kind, Key: k.key, Level: lvl})
	}
	slices.SortFunc(m.Sources, func(a, b Source) int {
		if a.Kind != b.Kind {
			if a.Kind < b.Kind {
				return -1
			}
			return 1
		}
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		}
		return 0
	})
	return m
}

// RoomLevel is a room's own level.
func RoomLevel(s v1alpha1.RoomSpec) v1alpha1.RoomDefaults {
	return v1alpha1.RoomDefaults{Env: s.Env, SecretEnv: s.SecretEnv, MCPServers: s.MCPServers, Network: s.Network}
}

// WorkspaceLevel is the workspace's level, mapping the deprecated
// internet: false to network mode none when no network is set.
func WorkspaceLevel(w *v1alpha1.Workspace) v1alpha1.RoomDefaults {
	d := w.Spec.RoomDefaults
	if d.Network == nil && w.Spec.Internet != nil && !*w.Spec.Internet { //nolint:staticcheck // the deprecated field still means mode none
		d.Network = &v1alpha1.NetworkAccess{Mode: v1alpha1.NetworkNone}
	}
	return d
}

// For merges the levels of a room.
func For(w *v1alpha1.Workspace, role *v1alpha1.RoleTemplate, room *v1alpha1.Room) Merged {
	var rd v1alpha1.RoomDefaults
	if role != nil {
		rd = role.Spec.RoomDefaults
	}
	return Merge(WorkspaceLevel(w), rd, RoomLevel(room.Spec))
}

// Selects reports whether a shared-volume selector (everyone, role:<t>,
// group:<g>, room:<name>) matches a room.
func Selects(sel, room, role, group string) bool {
	switch {
	case sel == "everyone":
		return true
	case sel == "role:"+role, sel == "room:"+room:
		return true
	case group != "" && sel == "group:"+group:
		return true
	}
	return false
}

// Walled reports whether links put a wall between groups a and b: a
// mediated or none link between them, in either direction.
func Walled(links []v1alpha1.GroupLink, a, b string) bool {
	if a == b {
		return false
	}
	for _, l := range links {
		if (l.From == a && l.To == b) || (l.From == b && l.To == a) {
			if l.Mode == v1alpha1.LinkMediated || l.Mode == v1alpha1.LinkNone {
				return true
			}
		}
	}
	return false
}
