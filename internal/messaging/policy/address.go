// Package policy decides who may message whom (docs/design.md §7.1, §7.4).
// It is pure: callers pass a directory snapshot and a people lookup.
package policy

import (
	"fmt"
	"regexp"
	"strings"
)

// nameRE is a valid room name, person handle, group or workspace. Names end
// up as NATS subject tokens and consumer names, so no dots.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidName reports whether s can be a name, group or workspace.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Target is a parsed address.
type Target struct {
	Name      string // a room or person, or a group if Group
	Workspace string
	Group     bool
}

// String formats the target as an address.
func (t Target) String() string {
	if t.Group {
		return "@" + t.Name + "@" + t.Workspace
	}
	return t.Name + "@" + t.Workspace
}

// Address formats name@workspace.
func Address(name, workspace string) string { return name + "@" + workspace }

// ParseAddress parses name, name@workspace, @group or @group@workspace. A
// missing workspace is defaultWS.
func ParseAddress(s, defaultWS string) (Target, error) {
	var t Target
	if strings.HasPrefix(s, "@") {
		t.Group = true
		s = s[1:]
	}
	name, ws, found := strings.Cut(s, "@")
	if !found {
		ws = defaultWS
	}
	if strings.Contains(ws, ".") {
		return t, fmt.Errorf("%q: addresses in other clusters (name@workspace.cluster) are not supported yet", s)
	}
	if !ValidName(name) {
		return t, fmt.Errorf("%q is not a valid address: use name, name@workspace or @group", s)
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
