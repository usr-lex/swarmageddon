package directory

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Defaults for channels and storm control (docs/design.md §7.8).
const (
	DefaultPostsPerMinute  = 20
	DefaultPushPerMinute   = 6
	DefaultCoalesceBytes   = 4096
	DefaultDigestInterval  = 15 * time.Minute
	DefaultFanoutPerMinute = 60
)

// ReservedScopes can't be channel names: they are reading scopes.
var ReservedScopes = []string{"all", "direct", "mentions"}

// Access is what a member may do in a channel. Several selector entries may
// match one member; the access is their union, which is the highest one.
type Access struct{ Read, Write bool }

// AccessOf converts the API's access level.
func AccessOf(a v1alpha1.ChannelAccess) Access {
	switch a {
	case v1alpha1.AccessRead:
		return Access{Read: true}
	case v1alpha1.AccessWrite:
		return Access{Write: true}
	case v1alpha1.AccessReadWrite:
		return Access{Read: true, Write: true}
	}
	return Access{}
}

// String is read, write, readWrite, or "" for none.
func (a Access) String() string {
	switch {
	case a.Read && a.Write:
		return string(v1alpha1.AccessReadWrite)
	case a.Read:
		return string(v1alpha1.AccessRead)
	case a.Write:
		return string(v1alpha1.AccessWrite)
	}
	return ""
}

func (a Access) union(b Access) Access { return Access{Read: a.Read || b.Read, Write: a.Write || b.Write} }

// ChannelMembers is a channel resolved against the workspace.
type ChannelMembers struct {
	// Access by member name: rooms and people.
	Access map[string]Access
	// People are the members that are people.
	People map[string]bool
	// Scope is the groups whose rooms may be members (the wall rule).
	Scope []string
	// Dropped are rooms a selector matched outside Scope, sorted.
	Dropped []string
}

// Channel returns the channel's spec, or nil.
func (w *Workspace) Channel(name string) *v1alpha1.ChannelSpec {
	for i := range w.Messaging.Channels {
		if w.Messaging.Channels[i].Name == name {
			return &w.Messaging.Channels[i]
		}
	}
	return nil
}

// OpenBothWays reports whether rooms of groups a and b may share a channel:
// the same group, or open links in both directions. Mediated links never
// carry channel traffic.
func (w *Workspace) OpenBothWays(a, b string) bool {
	if a == b {
		return true
	}
	open := func(from, to string) bool {
		for _, l := range w.Messaging.Links {
			if l.From == from && l.To == to {
				return l.Mode == v1alpha1.LinkOpen
			}
		}
		return false
	}
	return open(a, b) && open(b, a)
}

// Walled reports whether some groups of the workspace can't share channels.
func (w *Workspace) Walled() bool {
	groups := w.AllGroups()
	for i, a := range groups {
		for _, b := range groups[i+1:] {
			if !w.OpenBothWays(a, b) {
				return true
			}
		}
	}
	return false
}

// AllGroups lists declared groups and the groups of rooms, sorted. The
// default group (rooms without one) is "" and only listed if it has rooms.
func (w *Workspace) AllGroups() []string {
	seen := map[string]bool{}
	for _, g := range w.Groups {
		seen[g] = true
	}
	for _, r := range w.Rooms {
		seen[r.Group] = true
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// ParseSelector splits a selector into its kind and argument: everyone,
// people, or role|team|group|room|person:<name>.
func ParseSelector(sel string) (kind, arg string, err error) {
	if sel == "everyone" || sel == "people" {
		return sel, "", nil
	}
	kind, arg, ok := strings.Cut(sel, ":")
	if !ok || arg == "" || !slices.Contains([]string{"role", "team", "group", "room", "person"}, kind) {
		return "", "", fmt.Errorf("%q is not a selector: use everyone, people, role:<template>, team:<team>, group:<group>, room:<name> or person:<handle>", sel)
	}
	return kind, arg, nil
}

// MatchRooms returns the rooms a selector matches, sorted. people and
// person: match no rooms. An unknown role, team, group or room is an error.
func (w *Workspace) MatchRooms(sel string) ([]string, error) {
	kind, arg, err := ParseSelector(sel)
	if err != nil {
		return nil, err
	}
	var match func(Room) bool
	switch kind {
	case "everyone":
		match = func(Room) bool { return true }
	case "people", "person":
		return nil, nil
	case "role":
		if !w.Roles[arg] && !w.anyRoom(func(r Room) bool { return r.Role == arg }) {
			return nil, fmt.Errorf("there is no role template %s in workspace %s", arg, w.Name)
		}
		match = func(r Room) bool { return r.Role == arg }
	case "team":
		if !w.Teams[arg] && !w.anyRoom(func(r Room) bool { return slices.Contains(r.Teams, arg) }) {
			return nil, fmt.Errorf("there is no forge team %s in workspace %s", arg, w.Name)
		}
		match = func(r Room) bool { return slices.Contains(r.Teams, arg) }
	case "group":
		if !w.HasGroup(arg) {
			return nil, fmt.Errorf("there is no group %s in workspace %s", arg, w.Name)
		}
		match = func(r Room) bool { return r.Group == arg }
	case "room":
		if _, ok := w.Rooms[arg]; !ok {
			return nil, fmt.Errorf("there is no room %s in workspace %s", arg, w.Name)
		}
		return []string{arg}, nil
	}
	var out []string
	for _, name := range w.RoomNames() {
		if match(w.Rooms[name]) {
			out = append(out, name)
		}
	}
	return out, nil
}

func (w *Workspace) anyRoom(f func(Room) bool) bool {
	for _, r := range w.Rooms {
		if f(r) {
			return true
		}
	}
	return false
}

// ResolveChannel resolves a channel's members. people are the workspace's
// known people. Unresolvable selectors match no one. Rooms are members only
// within the channel's scope: the groups that open links connect both ways,
// chosen greedily from groups named by group: and room: selectors first,
// then from the groups with the most matched rooms.
func (w *Workspace) ResolveChannel(ch *v1alpha1.ChannelSpec, people []string) *ChannelMembers {
	out := &ChannelMembers{Access: map[string]Access{}, People: map[string]bool{}}
	rooms := map[string]Access{}
	var explicit []string // groups named by group: and room: selectors
	for _, m := range ch.Members {
		acc := AccessOf(m.Access)
		kind, arg, err := ParseSelector(m.Select)
		if err != nil {
			continue
		}
		switch kind {
		case "everyone", "people":
			for _, p := range people {
				if _, isRoom := w.Rooms[p]; !isRoom {
					out.People[p] = true
					out.Access[p] = out.Access[p].union(acc)
				}
			}
		case "person":
			if _, isRoom := w.Rooms[arg]; !isRoom {
				out.People[arg] = true
				out.Access[arg] = out.Access[arg].union(acc)
			}
		}
		if kind == "group" && !slices.Contains(explicit, arg) {
			explicit = append(explicit, arg)
		}
		if r, ok := w.Rooms[arg]; ok && kind == "room" && !slices.Contains(explicit, r.Group) {
			explicit = append(explicit, r.Group)
		}
		matched, _ := w.MatchRooms(m.Select)
		for _, r := range matched {
			rooms[r] = rooms[r].union(acc)
		}
	}
	count := map[string]int{}
	for r := range rooms {
		count[w.Rooms[r].Group]++
	}
	order := append([]string(nil), explicit...)
	var rest []string
	for g := range count {
		if !slices.Contains(order, g) {
			rest = append(rest, g)
		}
	}
	sort.Slice(rest, func(i, j int) bool {
		if count[rest[i]] != count[rest[j]] {
			return count[rest[i]] > count[rest[j]]
		}
		return rest[i] < rest[j]
	})
	for _, g := range append(order, rest...) {
		ok := true
		for _, s := range out.Scope {
			if !w.OpenBothWays(g, s) {
				ok = false
				break
			}
		}
		if ok {
			out.Scope = append(out.Scope, g)
		}
	}
	for r, acc := range rooms {
		if slices.Contains(out.Scope, w.Rooms[r].Group) {
			out.Access[r] = acc
		} else {
			out.Dropped = append(out.Dropped, r)
		}
	}
	sort.Strings(out.Dropped)
	return out
}

// Readers returns the members with read access, sorted.
func (c *ChannelMembers) Readers() []string {
	var out []string
	for n, a := range c.Access {
		if a.Read {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// DefaultDelivery is the channel's default delivery mode.
func DefaultDelivery(ch *v1alpha1.ChannelSpec) v1alpha1.DeliveryMode {
	if ch.DefaultDelivery != "" {
		return ch.DefaultDelivery
	}
	return v1alpha1.DeliveryDigest
}

// AllowedDelivery lists the modes readers may choose.
func AllowedDelivery(ch *v1alpha1.ChannelSpec) []v1alpha1.DeliveryMode {
	if len(ch.AllowedDelivery) > 0 {
		return ch.AllowedDelivery
	}
	return []v1alpha1.DeliveryMode{v1alpha1.DeliveryPush, v1alpha1.DeliveryDigest, v1alpha1.DeliveryPull}
}

// PostsPerMinute is the channel's post rate across all posters.
func PostsPerMinute(ch *v1alpha1.ChannelSpec) int {
	if ch.PostsPerMinute > 0 {
		return int(ch.PostsPerMinute)
	}
	return DefaultPostsPerMinute
}

// PushPerMinute is the per-recipient push budget.
func (w *Workspace) PushPerMinute() int {
	if n := w.Messaging.Storm.PushPerMinute; n > 0 {
		return int(n)
	}
	return DefaultPushPerMinute
}

// DigestInterval is the shortest time between two digests of one channel.
func (w *Workspace) DigestInterval() time.Duration {
	if d := w.Messaging.Storm.DigestInterval; d != nil && d.Duration > 0 {
		return d.Duration
	}
	return DefaultDigestInterval
}

// FanoutPerMinute is how many recipients a sender reaches per minute with
// group sends.
func (w *Workspace) FanoutPerMinute() int {
	if n := w.Messaging.Storm.FanoutPerMinute; n > 0 {
		return int(n)
	}
	return DefaultFanoutPerMinute
}

// ValidateChannels checks the workspace's channels: unique, unreserved names,
// resolvable selectors, delivery settings and the wall rule. problems make
// the channels invalid; notes report rooms a broad selector (everyone, role:,
// team:) matched outside a channel's scope, which are not members.
func ValidateChannels(w *Workspace) (problems, notes []string) {
	seen := map[string]bool{}
	for i := range w.Messaging.Channels {
		ch := &w.Messaging.Channels[i]
		name := "#" + ch.Name
		if seen[ch.Name] {
			problems = append(problems, fmt.Sprintf("%s is defined twice", name))
		}
		seen[ch.Name] = true
		if slices.Contains(ReservedScopes, ch.Name) {
			problems = append(problems, fmt.Sprintf("%s: %s is reserved for reading (read_messages scope); pick another name", name, ch.Name))
		}
		if len(ch.Members) == 0 {
			problems = append(problems, fmt.Sprintf("%s has no members", name))
		}
		for _, m := range ch.Members {
			if _, err := w.MatchRooms(m.Select); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			}
			if AccessOf(m.Access) == (Access{}) {
				problems = append(problems, fmt.Sprintf("%s: %s has no access level (read, write or readWrite)", name, m.Select))
			}
		}
		if len(ch.AllowedDelivery) > 0 && !slices.Contains(ch.AllowedDelivery, DefaultDelivery(ch)) {
			problems = append(problems, fmt.Sprintf("%s: the default delivery %s is not among the allowed ones", name, DefaultDelivery(ch)))
		}
		res := w.ResolveChannel(ch, nil)
		if len(res.Dropped) == 0 {
			continue
		}
		// Rooms or groups named explicitly outside the scope break the wall rule.
		var explicit []string
		for _, m := range ch.Members {
			kind, arg, err := ParseSelector(m.Select)
			if err != nil {
				continue
			}
			switch {
			case kind == "group" && !slices.Contains(res.Scope, arg):
				explicit = append(explicit, m.Select)
			case kind == "room" && slices.Contains(res.Dropped, arg):
				explicit = append(explicit, m.Select)
			}
		}
		scope := groupList(res.Scope)
		if len(explicit) > 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: %s can't share a channel with %s: groups need open links both ways (mediated links never carry channel traffic)",
				name, strings.Join(explicit, ", "), scope))
			continue
		}
		notes = append(notes, fmt.Sprintf("%s is limited to %s; %s outside it are not members", name, scope, strings.Join(res.Dropped, ", ")))
	}
	return problems, notes
}

func groupList(gs []string) string {
	out := make([]string, len(gs))
	for i, g := range gs {
		if g == "" {
			g = "(no group)"
		}
		out[i] = "group " + g
	}
	return strings.Join(out, " and ")
}
