package wizard

import (
	"strings"
	"testing"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

func names(cs []*ccv1.ChannelPlan) string {
	var n []string
	for _, c := range cs {
		n = append(n, c.Name)
	}
	return strings.Join(n, ",")
}

func TestDefaultChannels(t *testing.T) {
	p := &ccv1.WorkspacePlan{Agents: []*ccv1.AgentPlan{
		{Name: "a", Role: "coordinator"}, {Name: "b", Role: "implementer"}, {Name: "c", Role: "implementer"},
		{Name: "d", Role: "reviewer"}, {Name: "e", Role: "reviewer"}, {Name: "f", Role: "qa"},
	}}
	got := DefaultChannels(p)
	if names(got) != "announcements,general,implementers,reviewers" {
		t.Fatalf("defaults %s", names(got))
	}
	if got[0].DefaultDelivery != "push" || got[1].DefaultDelivery != "digest" || got[2].Members[0].Select != "role:implementer" {
		t.Fatalf("defaults %+v", got)
	}
	// Walled: per group, never across the wall.
	p.Groups = []*ccv1.GroupPlan{{Name: "clean"}, {Name: "dirty"}}
	p.Links = []*ccv1.LinkPlan{{From: "clean", To: "dirty", Mode: "mediated"}}
	if n := names(DefaultChannels(p)); n != "announcements-clean,general-clean,announcements-dirty,general-dirty" {
		t.Fatalf("walled defaults %s", n)
	}
	// The defaults validate.
	p.Channels = DefaultChannels(p)
	p.Name, p.Org = "acme", "acme"
	for _, a := range p.Agents {
		a.Cli = "claude"
	}
	if probs := validateChannels(p); len(probs) != 0 {
		t.Fatalf("defaults invalid: %v", probs)
	}
	p.Channels = append(p.Channels, &ccv1.ChannelPlan{Name: "Bad!", Members: []*ccv1.ChannelMemberPlan{{Select: "nobody", Access: "all"}}, DefaultDelivery: "shout"})
	p.Storm = &ccv1.StormPlan{DigestInterval: "often"}
	p.WardenMode = "paranoid"
	if probs := strings.Join(validateChannels(p), "\n"); !strings.Contains(probs, "selector") || !strings.Contains(probs, "access") ||
		!strings.Contains(probs, "delivery") || !strings.Contains(probs, "digest interval") || !strings.Contains(probs, "warden") {
		t.Fatalf("problems:\n%s", probs)
	}
}
