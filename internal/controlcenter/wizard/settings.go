package wizard

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/roomsettings"
)

// Settings levels, in merge order (docs/design.md §6.6).
const (
	LevelWorkspace = roomsettings.LevelWorkspace
	LevelRole      = roomsettings.LevelRole
	LevelRoom      = roomsettings.LevelRoom
)

// RoomSettings returns a room's own level of layered settings.
func RoomSettings(s v1alpha1.RoomSpec) v1alpha1.RoomDefaults { return roomsettings.RoomLevel(s) }

// SetRoomSettings writes a level into a room's spec.
func SetRoomSettings(s *v1alpha1.RoomSpec, d v1alpha1.RoomDefaults) {
	s.Env, s.SecretEnv, s.MCPServers, s.Network = d.Env, d.SecretEnv, d.MCPServers, d.Network
}

// Effective merges a room's levels (internal/roomsettings, as the operator
// does) into the API shape.
func Effective(room, role string, ws, rt, own v1alpha1.RoomDefaults) *ccv1.EffectiveSettings {
	m := roomsettings.Merge(ws, rt, own)
	var src []*ccv1.SettingSource
	for _, s := range m.Sources {
		src = append(src, &ccv1.SettingSource{Kind: s.Kind, Key: s.Key, Level: s.Level})
	}
	return &ccv1.EffectiveSettings{Room: room, Role: role, Settings: SettingsToPlan(m.Settings), Sources: src}
}

// SettingsFromPlan converts the API shape to the CRD shape; nil is empty.
func SettingsFromPlan(p *ccv1.RoomSettingsPlan) v1alpha1.RoomDefaults {
	var d v1alpha1.RoomDefaults
	if p == nil {
		return d
	}
	if len(p.GetEnv()) > 0 {
		d.Env = maps.Clone(p.GetEnv())
	}
	for _, e := range p.GetSecretEnv() {
		d.SecretEnv = append(d.SecretEnv, v1alpha1.SecretEnvVar{Name: e.GetName(), SecretRef: keyRef(e.GetSecret())})
	}
	if len(p.GetMcpServers()) > 0 {
		d.MCPServers = map[string]v1alpha1.MCPServer{}
		for k, s := range p.GetMcpServers() {
			d.MCPServers[k] = v1alpha1.MCPServer{Type: s.GetType(), Command: s.GetCommand(), Args: s.GetArgs(), Env: valuesFromPlan(s.GetEnv()),
				URL: s.GetUrl(), Headers: valuesFromPlan(s.GetHeaders()), EnabledTools: s.GetEnabledTools(), Disabled: s.GetDisabled()}
		}
	}
	if n := p.GetNetwork(); n != nil {
		d.Network = &v1alpha1.NetworkAccess{Mode: v1alpha1.NetworkMode(n.GetMode()), Domains: n.GetDomains()}
		if n.PackageManagers != nil {
			pm := n.GetPackageManagers()
			d.Network.PackageManagers = &pm
		}
	}
	return d
}

// SettingsToPlan converts the CRD shape to the API shape.
func SettingsToPlan(d v1alpha1.RoomDefaults) *ccv1.RoomSettingsPlan {
	p := &ccv1.RoomSettingsPlan{}
	if len(d.Env) > 0 {
		p.Env = maps.Clone(d.Env)
	}
	for _, e := range d.SecretEnv {
		p.SecretEnv = append(p.SecretEnv, &ccv1.SecretEnvPlan{Name: e.Name, Secret: &ccv1.SecretKeyPlan{Name: e.SecretRef.Name, Key: e.SecretRef.Key}})
	}
	if len(d.MCPServers) > 0 {
		p.McpServers = map[string]*ccv1.McpServerPlan{}
		for k, s := range d.MCPServers {
			p.McpServers[k] = &ccv1.McpServerPlan{Type: s.Type, Command: s.Command, Args: s.Args, Env: valuesToPlan(s.Env), Url: s.URL,
				Headers: valuesToPlan(s.Headers), EnabledTools: s.EnabledTools, Disabled: s.Disabled}
		}
	}
	if n := d.Network; n != nil {
		p.Network = &ccv1.NetworkPlan{Mode: string(n.Mode), Domains: n.Domains, PackageManagers: n.PackageManagers}
	}
	return p
}

func keyRef(k *ccv1.SecretKeyPlan) v1alpha1.SecretKeyRef {
	return v1alpha1.SecretKeyRef{Name: k.GetName(), Key: k.GetKey()}
}

func valuesFromPlan(m map[string]*ccv1.McpValuePlan) map[string]v1alpha1.MCPValue {
	if len(m) == 0 {
		return nil
	}
	out := map[string]v1alpha1.MCPValue{}
	for k, v := range m {
		mv := v1alpha1.MCPValue{Value: v.GetValue()}
		if v.GetValueFrom() != nil {
			r := keyRef(v.GetValueFrom())
			mv.ValueFrom = &r
		}
		out[k] = mv
	}
	return out
}

func valuesToPlan(m map[string]v1alpha1.MCPValue) map[string]*ccv1.McpValuePlan {
	if len(m) == 0 {
		return nil
	}
	out := map[string]*ccv1.McpValuePlan{}
	for k, v := range m {
		mv := &ccv1.McpValuePlan{Value: v.Value}
		if v.ValueFrom != nil {
			mv.ValueFrom = &ccv1.SecretKeyPlan{Name: v.ValueFrom.Name, Key: v.ValueFrom.Key}
		}
		out[k] = mv
	}
	return out
}

func isEmpty(d v1alpha1.RoomDefaults) bool {
	return len(d.Env) == 0 && len(d.SecretEnv) == 0 && len(d.MCPServers) == 0 && d.Network == nil
}

var (
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	dnsNameRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	domainRE    = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*(:[0-9]{1,5})?$`)
	volNameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	mountSelRE  = regexp.MustCompile(`^(everyone|(role|group|room):[a-z0-9][a-z0-9_-]{0,62})$`)
	// Values that look like credentials don't belong in plain env (design §11.4).
	keyLikeRE = regexp.MustCompile(`^(sk-|ghp_|gho_|github_pat_|glpat-|xox[abpr]-|AKIA)[A-Za-z0-9_-]{8,}|-----BEGIN `)
)

// ValidateSettings returns problems in one level of settings; where names the
// level for messages.
func ValidateSettings(where string, p *ccv1.RoomSettingsPlan) []string {
	if p == nil {
		return nil
	}
	var probs []string
	bad := func(f string, a ...any) { probs = append(probs, where+": "+fmt.Sprintf(f, a...)) }
	ref := func(what string, k *ccv1.SecretKeyPlan) {
		if !dnsNameRE.MatchString(k.GetName()) || !secretKeyRE.MatchString(k.GetKey()) {
			bad("%s needs a Secret name and key", what)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.GetEnv())) {
		if !envNameRE.MatchString(k) {
			bad("variable %q is not a valid name", k)
		}
		if keyLikeRE.MatchString(p.GetEnv()[k]) {
			bad("variable %q looks like a secret; use a secret variable instead", k)
		}
	}
	seen := map[string]bool{}
	for _, e := range p.GetSecretEnv() {
		if !envNameRE.MatchString(e.GetName()) {
			bad("secret variable %q is not a valid name", e.GetName())
		}
		if seen[e.GetName()] {
			bad("secret variable %q appears twice", e.GetName())
		}
		if _, ok := p.GetEnv()[e.GetName()]; ok {
			bad("variable %q is both plain and secret", e.GetName())
		}
		seen[e.GetName()] = true
		ref(fmt.Sprintf("secret variable %q", e.GetName()), e.GetSecret())
	}
	for _, name := range slices.Sorted(maps.Keys(p.GetMcpServers())) {
		s := p.GetMcpServers()[name]
		if name == "swarm" {
			bad(`MCP server "swarm" is reserved`)
		}
		if s.GetDisabled() {
			continue
		}
		switch s.GetType() {
		case "", "stdio":
			if s.GetCommand() == "" {
				bad("MCP server %q needs a command", name)
			}
		case "http":
			if s.GetUrl() == "" {
				bad("MCP server %q needs a URL", name)
			}
		default:
			bad("MCP server %q: type must be stdio or http", name)
		}
		for _, vals := range []map[string]*ccv1.McpValuePlan{s.GetEnv(), s.GetHeaders()} {
			for _, k := range slices.Sorted(maps.Keys(vals)) {
				v := vals[k]
				if v.GetValueFrom() != nil {
					if v.GetValue() != "" {
						bad("MCP server %q: %s has both a value and a Secret", name, k)
					}
					ref(fmt.Sprintf("MCP server %q %s", name, k), v.GetValueFrom())
				} else if keyLikeRE.MatchString(v.GetValue()) {
					bad("MCP server %q: %s looks like a secret; take it from a Secret", name, k)
				}
			}
		}
	}
	if n := p.GetNetwork(); n != nil {
		if n.GetMode() != "" && !slices.Contains([]string{"none", "limited", "full"}, n.GetMode()) {
			bad("network mode must be none, limited or full")
		}
		for _, d := range n.GetDomains() {
			if !domainRE.MatchString(d) {
				bad("domain %q is not a host or *.suffix", d)
			}
		}
	}
	return probs
}

// validateLayers checks every settings level and the shared volumes in a plan.
func validateLayers(p *ccv1.WorkspacePlan) []string {
	probs := ValidateSettings("workspace defaults", p.GetRoomDefaults())
	for _, r := range slices.Sorted(maps.Keys(p.GetRoleDefaults())) {
		probs = append(probs, ValidateSettings("role "+r, p.GetRoleDefaults()[r])...)
	}
	for _, a := range p.GetAgents() {
		probs = append(probs, ValidateSettings("agent "+a.GetName(), a.GetSettings())...)
	}
	return append(probs, validateVolumes(p)...)
}

// validateVolumes checks shared volumes, including that none is mounted
// read-write on both sides of a wall (a mediated or none link between groups).
func validateVolumes(p *ccv1.WorkspacePlan) []string {
	var probs []string
	seen := map[string]bool{}
	wall := map[[2]string]bool{}
	for _, l := range p.GetLinks() {
		if l.GetMode() == "mediated" || l.GetMode() == "none" {
			wall[[2]string{l.GetFrom(), l.GetTo()}] = true
			wall[[2]string{l.GetTo(), l.GetFrom()}] = true
		}
	}
	for _, v := range p.GetSharedVolumes() {
		n := v.GetName()
		if !volNameRE.MatchString(n) {
			probs = append(probs, fmt.Sprintf("shared volume %q: lower case letters, digits and dashes", n))
		}
		if seen[n] {
			probs = append(probs, fmt.Sprintf("shared volume %q appears twice", n))
		}
		seen[n] = true
		if q, err := resource.ParseQuantity(v.GetSize()); err != nil || q.Sign() <= 0 {
			probs = append(probs, fmt.Sprintf("shared volume %q: size %q is not a quantity", n, v.GetSize()))
		}
		if len(v.GetMounts()) == 0 {
			probs = append(probs, fmt.Sprintf("shared volume %q is mounted nowhere", n))
		}
		rwGroups := map[string]bool{}
		for _, m := range v.GetMounts() {
			if !mountSelRE.MatchString(m.GetSelect()) {
				probs = append(probs, fmt.Sprintf("shared volume %q: selector %q is not valid", n, m.GetSelect()))
				continue
			}
			if m.GetReadOnly() {
				continue
			}
			for _, a := range p.GetAgents() {
				if Selects(m.GetSelect(), a.GetName(), a.GetRole(), a.GetGroup()) {
					rwGroups[a.GetGroup()] = true
				}
			}
		}
		gs := slices.Sorted(maps.Keys(rwGroups))
		for i := range gs {
			for j := i + 1; j < len(gs); j++ {
				if wall[[2]string{gs[i], gs[j]}] {
					probs = append(probs, fmt.Sprintf("shared volume %q is read-write on both sides of the wall between %s and %s", n, gs[i], gs[j]))
				}
			}
		}
	}
	return probs
}

// Selects reports whether a shared-volume selector matches a room.
func Selects(sel, room, role, group string) bool { return roomsettings.Selects(sel, room, role, group) }

// RoomVolumes returns the shared volumes a room mounts: name -> "ro" or "rw"
// (read-write wins when several mounts match).
func RoomVolumes(vols []v1alpha1.SharedVolume, room, role, group string) map[string]string {
	out := map[string]string{}
	for _, v := range vols {
		for _, m := range v.Mounts {
			if !Selects(m.Select, room, role, group) {
				continue
			}
			if m.ReadOnly && out[v.Name] == "" {
				out[v.Name] = "ro"
			} else if !m.ReadOnly {
				out[v.Name] = "rw"
			}
		}
	}
	return out
}

func volumesFromPlan(p *ccv1.WorkspacePlan) []v1alpha1.SharedVolume {
	var out []v1alpha1.SharedVolume
	for _, v := range p.GetSharedVolumes() {
		sv := v1alpha1.SharedVolume{Name: v.GetName()}
		if q := qty(v.GetSize()); q != nil {
			sv.Size = *q
		}
		if v.GetStorageClass() != "" {
			sc := v.GetStorageClass()
			sv.StorageClassName = &sc
		}
		for _, m := range v.GetMounts() {
			sv.Mounts = append(sv.Mounts, v1alpha1.VolumeMount{Select: m.GetSelect(), ReadOnly: m.GetReadOnly()})
		}
		out = append(out, sv)
	}
	return out
}

func volumesToPlan(vols []v1alpha1.SharedVolume) []*ccv1.SharedVolumePlan {
	var out []*ccv1.SharedVolumePlan
	for _, v := range vols {
		sp := &ccv1.SharedVolumePlan{Name: v.Name, Size: v.Size.String()}
		if v.StorageClassName != nil {
			sp.StorageClass = *v.StorageClassName
		}
		for _, m := range v.Mounts {
			sp.Mounts = append(sp.Mounts, &ccv1.SharedVolumeMountPlan{Select: m.Select, ReadOnly: m.ReadOnly})
		}
		out = append(out, sp)
	}
	return out
}

// EffectiveAll returns the merged settings of every room in a desired state.
func EffectiveAll(d Desired) []*ccv1.EffectiveSettings {
	roles := map[string]v1alpha1.RoomDefaults{}
	for _, t := range d.RoleTemplates {
		roles[t.Name] = t.Spec.RoomDefaults
	}
	var out []*ccv1.EffectiveSettings
	for _, r := range d.Rooms {
		out = append(out, Effective(r.Name, r.Spec.Role, WorkspaceDefaults(d.Workspace), roles[r.Spec.Role], RoomSettings(r.Spec)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Room < out[j].Room })
	return out
}

// WorkspaceDefaults is the workspace level of room settings, mapping the
// deprecated internet: false to network mode none when no network is set.
func WorkspaceDefaults(w *v1alpha1.Workspace) v1alpha1.RoomDefaults {
	return roomsettings.WorkspaceLevel(w)
}

// EffectiveCurrent returns the merged settings of every existing room.
func EffectiveCurrent(cur Current) []*ccv1.EffectiveSettings {
	if cur.Workspace == nil {
		return nil
	}
	d := Desired{Workspace: cur.Workspace}
	for i := range cur.Rooms {
		d.Rooms = append(d.Rooms, &cur.Rooms[i])
	}
	for i := range cur.RoleTemplates {
		d.RoleTemplates = append(d.RoleTemplates, &cur.RoleTemplates[i])
	}
	return EffectiveAll(d)
}

// MissingTemplates names custom role templates the plan sets defaults for
// that don't exist (built-in ones are created by the operator).
func MissingTemplates(d Desired) []string {
	var out []string
	for _, t := range d.RoleTemplates {
		if t.ResourceVersion == "" && !slices.Contains(BuiltinRoles, t.Name) {
			out = append(out, fmt.Sprintf("role template %q does not exist", t.Name))
		}
	}
	return out
}
