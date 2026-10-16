package persona

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/roomsettings"
)

// Layered room settings (docs/design.md §6.6).

// SecretEnvKey is the identity Secret key holding a secretEnv variable.
func SecretEnvKey(name string) string { return "env_" + name }

// MergeFor merges the workspace's, the role's and the room's levels with
// internal/roomsettings, then lets a secret variable win over a plain one of
// the same name and drops the platform's own variables.
func MergeFor(ws *v1.Workspace, role *v1.RoleTemplate, r *v1.Room) v1.RoomDefaults {
	return clean(roomsettings.For(ws, role, r).Settings)
}

func clean(out v1.RoomDefaults) v1.RoomDefaults {
	for _, s := range out.SecretEnv {
		delete(out.Env, s.Name)
	}
	for k := range out.Env {
		if Reserved(k) {
			delete(out.Env, k)
		}
	}
	out.SecretEnv = slices.DeleteFunc(out.SecretEnv, func(s v1.SecretEnvVar) bool { return Reserved(s.Name) })
	return out
}

// NetworkMode is the merged mode; full when no level sets one.
func NetworkMode(d v1.RoomDefaults) v1.NetworkMode {
	if d.Network == nil || d.Network.Mode == "" {
		return v1.NetworkFull
	}
	return d.Network.Mode
}

// Settings are the merged settings with Secret references resolved, as the
// persona renders them.
type Settings struct {
	// Env are plain variables.
	Env map[string]string
	// SecretEnv names the variables whose values are in the identity
	// Secret, at SecretEnvKey(name).
	SecretEnv []string
	// MCPServers, resolved.
	MCPServers map[string]MCPServer
	// Network for room.json.
	Network Network
}

// MCPServer is a resolved MCP server.
type MCPServer struct {
	Type         string            `json:"type"`
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	URL          string            `json:"url,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	EnabledTools []string          `json:"-"`
}

// Network is the room's network access as room.json gives it to the
// supervisor, which sets HTTPS_PROXY, HTTP_PROXY and NO_PROXY in limited mode.
type Network struct {
	Mode string `json:"mode"`
	// ProxyURL is the egress proxy, without credentials: the supervisor adds
	// the room's (identity Secret key egress_auth) as the password.
	ProxyURL string   `json:"proxyURL,omitempty"`
	NoProxy  []string `json:"noProxy,omitempty"`
	// Domains reachable through the proxy (package managers included).
	Domains []string `json:"domains,omitempty"`
}

// Resolver returns the value of a key of a Secret in the workspace namespace.
type Resolver func(ref v1.SecretKeyRef) (string, error)

// Resolve resolves d's Secret references. It returns the settings, the
// secretEnv values by variable name, and the references it could not
// resolve (their variables and values are left out).
func Resolve(d v1.RoomDefaults, get Resolver) (Settings, map[string]string, []error) {
	s := Settings{Env: d.Env}
	values := map[string]string{}
	var errs []error
	for _, v := range d.SecretEnv {
		val, err := get(v.SecretRef)
		if err != nil {
			errs = append(errs, fmt.Errorf("secretEnv %s: %w", v.Name, err))
			continue
		}
		values[v.Name] = val
		s.SecretEnv = append(s.SecretEnv, v.Name)
	}
	resolve := func(what string, m map[string]v1.MCPValue) map[string]string {
		if len(m) == 0 {
			return nil
		}
		out := map[string]string{}
		for k, v := range m {
			if v.ValueFrom == nil {
				out[k] = v.Value
				continue
			}
			val, err := get(*v.ValueFrom)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", what, k, err))
				continue
			}
			out[k] = val
		}
		return out
	}
	for _, name := range slices.Sorted(maps.Keys(d.MCPServers)) {
		m := d.MCPServers[name]
		typ := m.Type
		if typ == "" {
			typ = "stdio"
			if m.Command == "" && m.URL != "" {
				typ = "http"
			}
		}
		if s.MCPServers == nil {
			s.MCPServers = map[string]MCPServer{}
		}
		s.MCPServers[name] = MCPServer{
			Type: typ, Command: m.Command, Args: m.Args, URL: m.URL, EnabledTools: m.EnabledTools,
			Env:     resolve("mcpServers "+name+" env", m.Env),
			Headers: resolve("mcpServers "+name+" header", m.Headers),
		}
	}
	return s, values, errs
}

// NetworkFor fills the room.json network fields for the merged settings.
// proxyURL is the egress proxy's URL; noProxy the hosts reached directly.
func NetworkFor(d v1.RoomDefaults, proxyURL string, noProxy []string, packageManagers []string) Network {
	n := Network{Mode: string(NetworkMode(d))}
	if n.Mode != string(v1.NetworkLimited) {
		return n
	}
	n.ProxyURL, n.NoProxy = proxyURL, noProxy
	n.Domains = append(n.Domains, d.Network.Domains...)
	if d.Network.PackageManagers != nil && *d.Network.PackageManagers {
		for _, p := range packageManagers {
			if !slices.Contains(n.Domains, p) {
				n.Domains = append(n.Domains, p)
			}
		}
	}
	return n
}

// reservedEnv are variables the platform sets.
var reservedEnv = []string{"SWARM_ROOM", "SWARM_WORKSPACE", "ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", GatewayKeyEnv,
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"}

// Reserved reports whether a variable is set by the platform and can't be
// overridden by the layered settings.
func Reserved(name string) bool {
	return slices.Contains(reservedEnv, name) || strings.HasPrefix(name, "SWARM_")
}
