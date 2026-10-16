// Package policy decides who may message whom (docs/design.md §7.1, §7.4).
// It is pure: callers pass a directory snapshot and a people lookup.
package policy

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// nameRE is a valid room name, person handle, group or workspace. Names end
// up as NATS subject tokens and consumer names, so no dots.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidName reports whether s can be a name, group or workspace.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Target is a parsed address.
type Target struct {
	Name      string // a room or person; for a group send, the selector's argument
	Workspace string
	// Kind is set for a group send: role, team or group.
	Kind string
}

// Group reports whether the target is a group send.
func (t Target) Group() bool { return t.Kind != "" }

// Selector is a group send's selector, e.g. role:reviewer.
func (t Target) Selector() string { return t.Kind + ":" + t.Name }

// String formats the target as an address.
func (t Target) String() string {
	if t.Group() {
		return "@" + t.Selector() + "@" + t.Workspace
	}
	return t.Name + "@" + t.Workspace
}

// Address formats name@workspace.
func Address(name, workspace string) string { return name + "@" + workspace }

// GroupKinds are the selectors a group send may use (design §7.8).
var GroupKinds = []string{"role", "team", "group"}

// ParseAddress parses name, name@workspace, or a group send: @role:<role>,
// @team:<team> or @group:<group>, each optionally followed by @workspace. A
// missing workspace is defaultWS.
func ParseAddress(s, defaultWS string) (Target, error) {
	var t Target
	in := s
	if strings.HasPrefix(s, "@") {
		kind, rest, ok := strings.Cut(s[1:], ":")
		if !ok || !slices.Contains(GroupKinds, kind) {
			return t, fmt.Errorf("%q is not an address: a group send is @role:<role>, @team:<team> or @group:<group>; one recipient is name or name@workspace", in)
		}
		t.Kind, s = kind, rest
	}
	name, ws, found := strings.Cut(s, "@")
	if !found {
		ws = defaultWS
	}
	if strings.Contains(ws, ".") {
		return t, fmt.Errorf("%q: addresses in other clusters (name@workspace.cluster) are not supported yet", in)
	}
	if !ValidName(name) {
		return t, fmt.Errorf("%q is not a valid address: use name, name@workspace, or @role:, @team: or @group: for a group send", in)
	}
	if !ValidName(ws) {
		return t, fmt.Errorf("%q is not a valid workspace", ws)
	}
	t.Name, t.Workspace = name, ws
	return t, nil
}

// SplitAddress splits name@workspace, requiring both parts.
func SplitAddress(s string) (name, workspace string, err error) {
	name, workspace, ok := strings.Cut(s, "@")
	if !ok || !ValidName(name) || !ValidName(workspace) {
		return "", "", fmt.Errorf("%q is not name@workspace", s)
	}
	return name, workspace, nil
}
