package directory

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// walled has clean and dirty behind mediated links, and ops open both ways
// with clean.
func walled() *Workspace {
	return &Workspace{
		Name:   "acme",
		Groups: []string{"clean", "dirty", "ops"},
		Messaging: v1alpha1.MessagingPolicy{Links: []v1alpha1.GroupLink{
			{From: "clean", To: "dirty", Mode: v1alpha1.LinkMediated},
			{From: "dirty", To: "clean", Mode: v1alpha1.LinkMediated},
			{From: "clean", To: "ops", Mode: v1alpha1.LinkOpen},
			{From: "ops", To: "clean", Mode: v1alpha1.LinkOpen},
		}},
		Rooms: map[string]Room{
			"hermes":  {Name: "hermes", Group: "clean", Role: "coordinator", Teams: []string{"mergers"}},
			"athena":  {Name: "athena", Group: "clean", Role: "reviewer", Teams: []string{"reviewers", "mergers"}},
			"themis":  {Name: "themis", Group: "clean", Role: "reviewer", Teams: []string{"reviewers"}},
			"metis":   {Name: "metis", Group: "dirty", Role: "reviewer", Teams: []string{"reviewers"}},
			"hecate":  {Name: "hecate", Group: "dirty", Role: "coordinator"},
			"atlas":   {Name: "atlas", Group: "ops", Role: "ci-infra"},
			"orphan":  {Name: "orphan", Role: "reviewer"},
			"ophelia": {Name: "ophelia", Group: "ops", Role: "reviewer"},
		},
		Roles: map[string]bool{"coordinator": true, "reviewer": true, "ci-infra": true, "qa": true},
		Teams: map[string]bool{"mergers": true, "reviewers": true, "ci": true},
	}
}

func members(c *ChannelMembers) string {
	var out []string
	for _, n := range slices.Sorted(maps.Keys(c.Access)) {
		out = append(out, n+"="+c.Access[n].String())
	}
	return strings.Join(out, " ")
}

func TestResolveChannel(t *testing.T) {
	ws := walled()
	people := []string{"alice", "bob"}
	ch := func(m ...string) *v1alpha1.ChannelSpec {
		c := &v1alpha1.ChannelSpec{Name: "x"}
		for i := 0; i < len(m); i += 2 {
			c.Members = append(c.Members, v1alpha1.ChannelMember{Select: m[i], Access: v1alpha1.ChannelAccess(m[i+1])})
		}
		return c
	}
	tests := []struct {
		name    string
		ch      *v1alpha1.ChannelSpec
		want    string
		dropped string
	}{
		{"role and team, highest access wins", ch("role:reviewer", "read", "team:mergers", "write", "group:clean", "read"),
			"athena=readWrite hermes=readWrite ophelia=read themis=read", "metis,orphan"},
		{"people and persons", ch("people", "read", "person:carol", "readWrite", "person:alice", "write"),
			"alice=readWrite bob=read carol=readWrite", ""},
		{"everyone in a walled workspace takes the biggest open scope", ch("everyone", "readWrite"),
			"alice=readWrite athena=readWrite atlas=readWrite bob=readWrite hermes=readWrite ophelia=readWrite themis=readWrite",
			"hecate,metis,orphan"},
		{"an explicit group anchors the scope", ch("group:dirty", "readWrite", "role:coordinator", "write"),
			"hecate=readWrite metis=readWrite", "hermes"},
		{"room selector", ch("room:atlas", "read", "room:hermes", "write"), "atlas=read hermes=write", ""},
		{"unknown selectors match no one", ch("role:ghost", "read", "room:ghost", "read"), "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ws.ResolveChannel(tc.ch, people)
			if members(got) != tc.want || strings.Join(got.Dropped, ",") != tc.dropped {
				t.Fatalf("members %q dropped %v; want %q dropped %s", members(got), got.Dropped, tc.want, tc.dropped)
			}
		})
	}
}

func TestValidateChannels(t *testing.T) {
	ws := walled()
	ws.Messaging.Channels = []v1alpha1.ChannelSpec{
		{Name: "general", Members: []v1alpha1.ChannelMember{{Select: "everyone", Access: "readWrite"}}},
		{Name: "general", Members: []v1alpha1.ChannelMember{{Select: "people", Access: "read"}}},
		{Name: "mentions", Members: []v1alpha1.ChannelMember{{Select: "people", Access: "read"}}},
		{Name: "bad", Members: []v1alpha1.ChannelMember{{Select: "role:ghost", Access: "read"}, {Select: "team:nobody", Access: "read"}, {Select: "group:mars", Access: "read"}}},
		{Name: "wall", Members: []v1alpha1.ChannelMember{{Select: "group:clean", Access: "readWrite"}, {Select: "room:metis", Access: "read"}}},
		{Name: "modes", DefaultDelivery: "push", AllowedDelivery: []v1alpha1.DeliveryMode{"digest", "pull"},
			Members: []v1alpha1.ChannelMember{{Select: "people", Access: "read"}}},
		{Name: "ok", Members: []v1alpha1.ChannelMember{{Select: "group:clean", Access: "readWrite"}, {Select: "group:ops", Access: "read"}}},
	}
	problems, notes := ValidateChannels(ws)
	all := strings.Join(problems, "\n")
	for _, want := range []string{
		"#general is defined twice",
		"#mentions: mentions is reserved",
		"#bad: there is no role template ghost",
		"#bad: there is no forge team nobody",
		"#bad: there is no group mars",
		"#wall: room:metis can't share a channel with group clean: groups need open links both ways (mediated links never carry channel traffic)",
		"#modes: the default delivery push is not among the allowed ones",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing problem %q in:\n%s", want, all)
		}
	}
	if len(problems) != 7 {
		t.Errorf("%d problems:\n%s", len(problems), all)
	}
	if len(notes) != 1 || notes[0] != "#general is limited to group clean and group ops; hecate, metis, orphan outside it are not members" {
		t.Errorf("notes %q", notes)
	}
	if !ws.Walled() {
		t.Error("not walled")
	}
	ws.Messaging.Links = nil
	ws.Groups = []string{"clean"}
	for n, r := range ws.Rooms {
		r.Group = "clean"
		ws.Rooms[n] = r
	}
	if ws.Walled() {
		t.Error(fmt.Sprint("walled with one group: ", ws.AllGroups()))
	}
}
