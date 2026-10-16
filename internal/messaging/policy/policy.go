package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
)

// Kind is the kind of a party: an agent (room) or a person.
type Kind string

const (
	Agent  Kind = "agent"
	Human  Kind = "human"
	System Kind = "system"
)

// Forge is the name of the system sender that relays forge activity
// (design §9.7). Only the control center may send as it; it is one-way.
const Forge = "forge"

// Status is the system address agents send status reports to (design
// §8.7). Only the control center reads its inbox.
const Status = "status"

// IsSystem reports whether name is a system address.
func IsSystem(name string) bool { return name == Forge || name == Status }

// ForgeRatePerMinute is the forge sender's rate limit per workspace.
const ForgeRatePerMinute = 120

// Party is a sender or recipient inside one workspace.
type Party struct {
	Name  string
	Kind  Kind
	Group string // agents only
}

// Code classifies a refusal; the server maps it to a Connect code.
type Code int

const (
	Denied    Code = iota // PermissionDenied
	Invalid               // InvalidArgument
	NotFound              // NotFound
	Exhausted             // ResourceExhausted
)

// Refusal says which rule refused a message and what would be allowed.
type Refusal struct {
	Code   Code
	Rule   string
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

func refuse(code Code, rule, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Rule: rule, Reason: fmt.Sprintf(format, args...)}
}

// IsPerson reports whether name is a person in the workspace.
type IsPerson func(name string) bool

// Route is where an allowed message goes.
type Route struct {
	Recipients []Party
	// Link names the group link crossed, e.g. "clean->dirty"; empty within a
	// group or to or from a person.
	Link     string
	Mediated bool
}

// Lookup finds a room or person by name. Rooms win over people.
func Lookup(ws *directory.Workspace, isPerson IsPerson, name string) (Party, bool) {
	if IsSystem(name) {
		return Party{Name: name, Kind: System}, true
	}
	if r, ok := ws.Rooms[name]; ok {
		return Party{Name: name, Kind: Agent, Group: r.Group}, true
	}
	if isPerson != nil && isPerson(name) {
		return Party{Name: name, Kind: Human}, true
	}
	return Party{}, false
}

// Check evaluates a message from `from` to address `to` with body `body`
// against the workspace's policy (§7.4 steps 2 to 4; identity is the
// caller's job, the rate limit the server's).
func Check(ws *directory.Workspace, isPerson IsPerson, from Party, to, body string) (*Route, *Refusal) {
	if n, max := len(body), ws.MaxBytes(); n > max {
		return nil, refuse(Invalid, "size", "the message is %d bytes; workspace %s allows at most %d", n, ws.Name, max)
	}
	t, err := ParseAddress(to, ws.Name)
	if err != nil {
		return nil, refuse(Invalid, "address", "%v", err)
	}
	if t.Workspace != ws.Name {
		return nil, refuse(Denied, "workspace", "messages stay inside workspace %s; %s is in workspace %s", ws.Name, t, t.Workspace)
	}
	if t.Group {
		if from.Kind == System {
			return nil, refuse(Invalid, "forge", "forge notices go to one agent at a time, not to groups")
		}
		return checkGroup(ws, from, t.Name, body)
	}
	if t.Name == Forge {
		return nil, refuse(Invalid, "forge", "forge notices are one-way; reply on the issue or PR")
	}
	rcpt, ok := Lookup(ws, isPerson, t.Name)
	if !ok {
		return nil, refuse(NotFound, "unknown-recipient", "there is no one named %s in workspace %s", t.Name, ws.Name)
	}
	if rcpt.Name == from.Name {
		return nil, refuse(Invalid, "self", "you can't message yourself")
	}
	link, r := pair(ws, from, rcpt)
	if r != nil {
		return nil, r
	}
	route := &Route{Recipients: []Party{rcpt}}
	if link != nil {
		if r := checkPrefix(link, rcpt.Name, body); r != nil {
			return nil, r
		}
		if link.From != link.To {
			route.Link = linkName(link)
		}
		route.Mediated = link.Mode == v1alpha1.LinkMediated
	}
	return route, nil
}

// Allowed reports whether from may message to, ignoring the body, with a note
// on any restriction (e.g. a required prefix). For ListPeers.
func Allowed(ws *directory.Workspace, from, to Party) (bool, string) {
	if from.Name == to.Name {
		return false, ""
	}
	link, r := pair(ws, from, to)
	if r != nil {
		return false, ""
	}
	if link == nil {
		return true, ""
	}
	var notes []string
	if link.Mode == v1alpha1.LinkMediated {
		notes = append(notes, fmt.Sprintf("mediated link %s: one named recipient per message, no group addresses", linkName(link)))
	}
	if link.RequirePrefix != "" {
		notes = append(notes, fmt.Sprintf("messages must start with %q", prefixFor(link, to.Name)))
	}
	return true, strings.Join(notes, "; ")
}

// checkGroup routes @group: everyone in the group the sender may reach.
func checkGroup(ws *directory.Workspace, from Party, group, body string) (*Route, *Refusal) {
	if !ws.HasGroup(group) {
		return nil, refuse(NotFound, "unknown-group", "there is no group %s in workspace %s", group, ws.Name)
	}
	var route Route
	var members []Party
	for _, name := range ws.RoomNames() {
		r := ws.Rooms[name]
		if r.Group == group && name != from.Name {
			members = append(members, Party{Name: name, Kind: Agent, Group: group})
		}
	}
	if len(members) == 0 {
		return nil, refuse(Invalid, "empty-group", "group %s has no one else in it", group)
	}
	if from.Kind == Agent && from.Group != group {
		l := findLink(ws, from.Group, group)
		if l != nil && l.Mode == v1alpha1.LinkMediated {
			return nil, refuse(Denied, "mediated-group",
				"group addresses can't cross the mediated link %s; name exactly one recipient%s",
				linkName(l), recipientsHint(l))
		}
	}
	for _, m := range members {
		link, r := pair(ws, from, m)
		if r != nil {
			continue
		}
		if link != nil {
			if r := checkPrefix(link, m.Name, body); r != nil {
				return nil, r
			}
			if link.From != link.To {
				route.Link = linkName(link)
			}
		}
		route.Recipients = append(route.Recipients, m)
	}
	if len(route.Recipients) == 0 {
		_, r := pair(ws, from, members[0])
		return nil, r
	}
	return &route, nil
}

// pair checks reachability between two parties and returns the link that
// governs it (nil when none applies: within a group by default, or people).
func pair(ws *directory.Workspace, from, to Party) (*v1alpha1.GroupLink, *Refusal) {
	if from.Kind == System {
		if from.Name != Forge {
			return nil, refuse(Denied, "system", "%s can't send messages", from.Name)
		}
		// The forge's own permissions already decided visibility (§9.7).
		if to.Kind != Agent {
			return nil, refuse(Invalid, "forge", "forge notices go to agents only; %s is not an agent", to.Name)
		}
		return nil, nil
	}
	if to.Kind == System {
		if to.Name == Status {
			if from.Kind != Agent {
				return nil, refuse(Denied, "status", "only agents send reports to %s@%s", Status, ws.Name)
			}
			return nil, nil // any agent of the workspace, no group link needed (§8.7)
		}
		return nil, refuse(Invalid, "forge", "forge notices are one-way; reply on the issue or PR")
	}
	if from.Kind == Human || to.Kind == Human {
		if from.Kind == Human && to.Kind == Human {
			return nil, nil
		}
		if !ws.HumansReachable() {
			return nil, refuse(Denied, "humans", "people and agents can't message each other in workspace %s (messaging.humansReachable is false)", ws.Name)
		}
		return nil, nil
	}
	if from.Group == to.Group && ws.WithinGroup() {
		return nil, nil
	}
	l := findLink(ws, from.Group, to.Group)
	if from.Group == to.Group && (l == nil || l.Mode == v1alpha1.LinkNone) {
		return nil, refuse(Denied, "within-group", "rooms in %s can't message each other (messaging.defaultLinks is none)", groupName(from.Group))
	}
	if l == nil {
		return nil, refuse(Denied, "no-link", "%s can't message %s: there is no link from %s to %s%s",
			from.Name, to.Name, groupName(from.Group), groupName(to.Group), reachableHint(ws, from))
	}
	switch l.Mode {
	case v1alpha1.LinkNone:
		return nil, refuse(Denied, "link-none", "%s can't message %s: the link %s is closed%s",
			from.Name, to.Name, linkName(l), reachableHint(ws, from))
	case v1alpha1.LinkMediated:
		if len(l.Senders) > 0 && !slices.Contains(l.Senders, from.Name) {
			if len(l.Recipients) > 0 && slices.Contains(l.Recipients, to.Name) {
				return nil, refuse(Denied, "mediated-sender", "only %s may message %s", names(l.Senders), to.Name)
			}
			return nil, refuse(Denied, "mediated-sender", "only %s may send across the mediated link %s", names(l.Senders), linkName(l))
		}
		if len(l.Recipients) > 0 && !slices.Contains(l.Recipients, to.Name) {
			return nil, refuse(Denied, "mediated-recipient", "across the mediated link %s, %s may only message %s",
				linkName(l), from.Name, names(l.Recipients))
		}
	}
	return l, nil
}

func checkPrefix(l *v1alpha1.GroupLink, recipient, body string) *Refusal {
	if l.RequirePrefix == "" {
		return nil
	}
	p := prefixFor(l, recipient)
	if !strings.HasPrefix(body, p) {
		return refuse(Invalid, "prefix", "messages across %s must start with %q", linkName(l), p)
	}
	return nil
}

func prefixFor(l *v1alpha1.GroupLink, recipient string) string {
	return strings.ReplaceAll(l.RequirePrefix, "{recipient}", recipient)
}

// findLink returns the first link from one group to another (one per direction).
func findLink(ws *directory.Workspace, from, to string) *v1alpha1.GroupLink {
	for i := range ws.Messaging.Links {
		l := &ws.Messaging.Links[i]
		if l.From == from && l.To == to {
			return l
		}
	}
	return nil
}

// reachableHint lists the groups the sender's group has links to.
func reachableHint(ws *directory.Workspace, from Party) string {
	var to []string
	if ws.WithinGroup() {
		to = append(to, groupName(from.Group))
	}
	for _, l := range ws.Messaging.Links {
		if l.From == from.Group && l.Mode != v1alpha1.LinkNone && !slices.Contains(to, groupName(l.To)) {
			to = append(to, groupName(l.To))
		}
	}
	if len(to) == 0 {
		return ""
	}
	return "; " + from.Name + " may message rooms in " + names(to)
}

func recipientsHint(l *v1alpha1.GroupLink) string {
	if len(l.Recipients) == 0 {
		return ""
	}
	return ": " + names(l.Recipients)
}

func linkName(l *v1alpha1.GroupLink) string { return groupName(l.From) + "->" + groupName(l.To) }

func groupName(g string) string {
	if g == "" {
		return "(no group)"
	}
	return g
}

// names joins "a", "a and b", "a, b and c".
func names(n []string) string {
	switch len(n) {
	case 0:
		return "no one"
	case 1:
		return n[0]
	}
	return strings.Join(n[:len(n)-1], ", ") + " and " + n[len(n)-1]
}
