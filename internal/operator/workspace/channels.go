package workspace

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
)

// ConditionChannelsValid reports whether spec.messaging.channels has unique
// names, resolvable selectors and keeps the wall rule (docs/design.md §7.8).
const ConditionChannelsValid = "ChannelsValid"

// AnnotationDefaultChannels marks a workspace whose default channels were
// created, so they are created once and a person may remove them.
const AnnotationDefaultChannels = "swarmageddon.dev/default-channels"

// CoordinatorRole is the role template whose rooms may write to the
// default channels.
const CoordinatorRole = "coordinator"

// reconcileChannels adds the default channels once, then validates the
// channels into ChannelsValid. It changes the Workspace's spec and
// annotations only when it adds the defaults.
func (r *Reconciler) reconcileChannels(ctx context.Context, w *v1alpha1.Workspace, ns string) error {
	var rooms v1alpha1.RoomList
	if err := r.List(ctx, &rooms, client.InNamespace(ns)); err != nil {
		return err
	}
	var roles v1alpha1.RoleTemplateList
	if err := r.List(ctx, &roles, client.InNamespace(ns)); err != nil {
		return err
	}
	dir := directory.FromObjects(w, rooms.Items, roles.Items)
	if _, done := w.Annotations[AnnotationDefaultChannels]; !done && len(w.Spec.Messaging.Channels) == 0 && len(dir.Rooms) > 0 {
		orig := w.DeepCopy()
		w.Spec.Messaging.Channels = DefaultChannels(dir)
		if w.Annotations == nil {
			w.Annotations = map[string]string{}
		}
		w.Annotations[AnnotationDefaultChannels] = "created"
		status := w.Status
		if err := r.Patch(ctx, w, client.MergeFrom(orig)); err != nil {
			return fmt.Errorf("add default channels: %w", err)
		}
		w.Status = status // Patch returns the stored object, without our status edits
		dir.Messaging = w.Spec.Messaging
	}
	problems, notes := directory.ValidateChannels(dir)
	switch {
	case len(problems) > 0:
		setCond(w, ConditionChannelsValid, false, "Invalid", strings.Join(problems, "; "))
	case len(notes) > 0:
		setCond(w, ConditionChannelsValid, true, "Scoped", strings.Join(notes, "; "))
	default:
		setCond(w, ConditionChannelsValid, true, "Valid", fmt.Sprintf("%d channels", len(w.Spec.Messaging.Channels)))
	}
	return nil
}

// DefaultChannels are the channels a new workspace starts with: an
// announcements channel, a general one, and one per role template with two
// or more rooms. In a walled workspace each zone of groups that open links
// join both ways gets its own set, suffixed with its first group's name.
func DefaultChannels(w *directory.Workspace) []v1alpha1.ChannelSpec {
	zones := zonesOf(w)
	if len(zones) <= 1 {
		return channelSet(w, "", "everyone", nil)
	}
	var out []v1alpha1.ChannelSpec
	for _, z := range zones {
		suffix := z[0]
		if suffix == "" {
			suffix = "ungrouped"
		}
		out = append(out, channelSet(w, "-"+suffix, "", z)...)
	}
	return out
}

// zonesOf groups the workspace's groups that have rooms, greedily, into
// sets whose members are joined by open links both ways.
func zonesOf(w *directory.Workspace) [][]string {
	used := map[string]bool{}
	for _, r := range w.Rooms {
		used[r.Group] = true
	}
	var zones [][]string
	for _, g := range w.AllGroups() {
		if !used[g] {
			continue
		}
		placed := false
		for i, z := range zones {
			if !slices.ContainsFunc(z, func(o string) bool { return !w.OpenBothWays(g, o) }) {
				zones[i] = append(z, g)
				placed = true
				break
			}
		}
		if !placed {
			zones = append(zones, []string{g})
		}
	}
	return zones
}

// channelSet builds one set of default channels. Rooms are selected by
// everyone, or by the zone's groups.
func channelSet(w *directory.Workspace, suffix, everyone string, groups []string) []v1alpha1.ChannelSpec {
	m := func(sel string, a v1alpha1.ChannelAccess) v1alpha1.ChannelMember {
		return v1alpha1.ChannelMember{Select: sel, Access: a}
	}
	all := func(a v1alpha1.ChannelAccess) []v1alpha1.ChannelMember {
		if everyone != "" {
			return []v1alpha1.ChannelMember{m(everyone, a)}
		}
		out := []v1alpha1.ChannelMember{m("people", a)}
		for _, g := range groups {
			if g == "" {
				// The default group has no selector: name its rooms.
				for _, n := range w.RoomNames() {
					if w.Rooms[n].Group == "" {
						out = append(out, m("room:"+n, a))
					}
				}
				continue
			}
			out = append(out, m("group:"+g, a))
		}
		return out
	}
	where := ""
	if len(groups) > 0 {
		where = " in " + strings.Join(groupNames(groups), ", ")
	}
	ann := v1alpha1.ChannelSpec{Name: "announcements" + suffix, Description: "Announcements for everyone" + where,
		DefaultDelivery: v1alpha1.DeliveryPush,
		Members: append(all(v1alpha1.AccessRead), m("people", v1alpha1.AccessWrite),
			m("role:"+CoordinatorRole, v1alpha1.AccessWrite))}
	gen := v1alpha1.ChannelSpec{Name: "general" + suffix, Description: "The running conversation" + where,
		DefaultDelivery: v1alpha1.DeliveryDigest, Members: all(v1alpha1.AccessReadWrite)}
	out := []v1alpha1.ChannelSpec{ann, gen}

	count := map[string]int{}
	for _, r := range w.Rooms {
		if r.Role != "" && (groups == nil || slices.Contains(groups, r.Group)) {
			count[r.Role]++
		}
	}
	var roles []string
	for role, n := range count {
		if n >= 2 {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	for _, role := range roles {
		name := role
		if !strings.HasSuffix(name, "s") {
			name += "s"
		}
		name += suffix
		if len(name) > 63 {
			continue
		}
		members := []v1alpha1.ChannelMember{m("role:"+role, v1alpha1.AccessReadWrite)}
		if role != CoordinatorRole {
			members = append(members, m("role:"+CoordinatorRole, v1alpha1.AccessWrite))
		}
		out = append(out, v1alpha1.ChannelSpec{Name: name, Description: "Rooms with the " + role + " role" + where,
			DefaultDelivery: v1alpha1.DeliveryDigest, Members: members})
	}
	// The coordinator selector may name a role no room has yet; that is fine
	// for validation as long as the role template exists.
	if !w.Roles[CoordinatorRole] {
		for i := range out {
			out[i].Members = slices.DeleteFunc(out[i].Members, func(m v1alpha1.ChannelMember) bool {
				return m.Select == "role:"+CoordinatorRole
			})
		}
	}
	return out
}

func groupNames(gs []string) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		if g == "" {
			g = "(no group)"
		}
		out[i] = g
	}
	return out
}
