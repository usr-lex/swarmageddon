package wizard

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

// DefaultChannels returns the channels created for a workspace whose plan
// lists none (docs/design.md §7.8):
//   - #announcements: everyone reads; people and coordinators write; push
//   - #general: everyone read-write; digest
//   - one channel per role with two or more rooms (e.g. #reviewers): that
//     role read-write, coordinators write; digest
//
// In a walled workspace (any mediated link) #announcements and #general are
// made per group instead (#general-<group>), since channels never cross a
// wall. The operator owns the real defaults; this mirrors them for the wizard.
func DefaultChannels(p *ccv1.WorkspacePlan) []*ccv1.ChannelPlan {
	walled := slices.ContainsFunc(p.GetLinks(), func(l *ccv1.LinkPlan) bool { return l.GetMode() == "mediated" })
	m := func(sel, access string) *ccv1.ChannelMemberPlan {
		return &ccv1.ChannelMemberPlan{Select: sel, Access: access}
	}
	var out []*ccv1.ChannelPlan
	if !walled {
		out = append(out,
			&ccv1.ChannelPlan{Name: "announcements", Description: "Announcements for everyone", DefaultDelivery: "push",
				Members: []*ccv1.ChannelMemberPlan{m("everyone", "read"), m("people", "readWrite"), m("role:coordinator", "readWrite")}},
			&ccv1.ChannelPlan{Name: "general", Description: "The whole team", DefaultDelivery: "digest",
				Members: []*ccv1.ChannelMemberPlan{m("everyone", "readWrite")}})
	} else {
		for _, g := range p.GetGroups() {
			out = append(out,
				&ccv1.ChannelPlan{Name: "announcements-" + g.GetName(), Description: "Announcements for group " + g.GetName(), DefaultDelivery: "push",
					Members: []*ccv1.ChannelMemberPlan{m("group:"+g.GetName(), "read"), m("people", "readWrite")}},
				&ccv1.ChannelPlan{Name: "general-" + g.GetName(), Description: "Group " + g.GetName(), DefaultDelivery: "digest",
					Members: []*ccv1.ChannelMemberPlan{m("group:"+g.GetName(), "readWrite"), m("people", "readWrite")}})
		}
	}
	count := map[string]int{}
	for _, a := range p.GetAgents() {
		count[a.GetRole()]++
	}
	var roles []string
	for r, n := range count {
		if n >= 2 {
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if !walled {
		for _, r := range roles {
			out = append(out, &ccv1.ChannelPlan{Name: plural(r), Description: "Rooms with role " + r, DefaultDelivery: "digest",
				Members: []*ccv1.ChannelMemberPlan{m("role:"+r, "readWrite"), m("role:coordinator", "write")}})
		}
	}
	return out
}

func plural(role string) string {
	switch role {
	case "qa", "ci-infra":
		return role
	}
	return role + "s"
}

var (
	channelName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	selectorRE  = regexp.MustCompile(`^(everyone|people|(role|team|group|room|person):[a-z0-9][a-z0-9_-]{0,62})$`)
	accesses    = []string{"read", "write", "readWrite"}
	deliveries  = []string{"push", "digest", "pull"}
)

// validateChannels returns problems in the plan's channels and storm settings.
func validateChannels(p *ccv1.WorkspacePlan) []string {
	var probs []string
	seen := map[string]bool{}
	for _, c := range p.GetChannels() {
		if !channelName.MatchString(c.GetName()) {
			probs = append(probs, fmt.Sprintf("channel %q: lower case letters, digits, - and _", c.GetName()))
		}
		if seen[c.GetName()] {
			probs = append(probs, fmt.Sprintf("channel %q appears twice", c.GetName()))
		}
		seen[c.GetName()] = true
		if len(c.GetMembers()) == 0 {
			probs = append(probs, fmt.Sprintf("channel %q has no members", c.GetName()))
		}
		for _, mb := range c.GetMembers() {
			if !selectorRE.MatchString(mb.GetSelect()) {
				probs = append(probs, fmt.Sprintf("channel %q: selector %q is not valid", c.GetName(), mb.GetSelect()))
			}
			if !slices.Contains(accesses, mb.GetAccess()) {
				probs = append(probs, fmt.Sprintf("channel %q: access must be read, write or readWrite", c.GetName()))
			}
		}
		if d := c.GetDefaultDelivery(); d != "" && !slices.Contains(deliveries, d) {
			probs = append(probs, fmt.Sprintf("channel %q: delivery must be push, digest or pull", c.GetName()))
		}
		for _, d := range c.GetAllowedDelivery() {
			if !slices.Contains(deliveries, d) {
				probs = append(probs, fmt.Sprintf("channel %q: delivery must be push, digest or pull", c.GetName()))
			}
		}
	}
	if st := p.GetStorm(); st != nil && st.GetDigestInterval() != "" {
		if _, err := time.ParseDuration(st.GetDigestInterval()); err != nil {
			probs = append(probs, "the digest interval is not a duration (e.g. 15m)")
		}
	}
	modes := []string{"off", "monitor", "gate"}
	if m := p.GetWardenMode(); m != "" && !slices.Contains(modes, m) {
		probs = append(probs, "the warden mode must be off, monitor or gate")
	}
	if w := p.GetWarden(); w != nil {
		if w.GetMode() != "" && !slices.Contains(modes, w.GetMode()) {
			probs = append(probs, "the warden mode must be off, monitor or gate")
		}
		if w.GetMonthlyUsd() < 0 {
			probs = append(probs, "the warden budget can't be negative")
		}
	}
	for _, a := range p.GetAgents() {
		if m := a.GetWardenMode(); m != "" && !slices.Contains(modes, m) {
			probs = append(probs, fmt.Sprintf("agent %q: the warden mode must be off, monitor or gate", a.GetName()))
		}
		probs = append(probs, validateAdvisor("agent "+a.GetName(), a.GetAdvisor())...)
	}
	for _, name := range slices.Sorted(maps.Keys(p.GetRoleAdvisors())) {
		probs = append(probs, validateAdvisor("role "+name, p.GetRoleAdvisors()[name])...)
	}
	return probs
}

func validateAdvisor(where string, a *ccv1.AdvisorPlan) []string {
	if a == nil {
		return nil
	}
	var probs []string
	if a.GetMaxCallsPerHour() < 0 || a.GetMonthlyUsd() < 0 {
		probs = append(probs, where+": advisor limits can't be negative")
	}
	return probs
}

// AdvisorFromPlan converts an advisor plan. Unset is nil (inherit). An
// empty model is kept for a room (it turns the role's advisor off) and is
// nil for a role template (no advisor).
func AdvisorFromPlan(a *ccv1.AdvisorPlan, room bool) *v1alpha1.AdvisorSpec {
	if a == nil || (a.GetModel() == "" && !room) {
		return nil
	}
	if a.GetModel() == "" {
		return &v1alpha1.AdvisorSpec{}
	}
	return &v1alpha1.AdvisorSpec{Model: a.GetModel(), Effort: a.GetEffort(), MaxCallsPerHour: a.GetMaxCallsPerHour(),
		MonthlyUSD: a.GetMonthlyUsd(), Native: a.GetNative()}
}

// AdvisorToPlan is the reverse of AdvisorFromPlan.
func AdvisorToPlan(a *v1alpha1.AdvisorSpec) *ccv1.AdvisorPlan {
	if a == nil {
		return nil
	}
	return &ccv1.AdvisorPlan{Model: a.Model, Effort: a.Effort, MaxCallsPerHour: a.MaxCallsPerHour, MonthlyUsd: a.MonthlyUSD, Native: a.Native}
}

// applyChannels sets channels, storm and warden settings on a spec.
func applyChannels(p *ccv1.WorkspacePlan, s *v1alpha1.WorkspaceSpec) {
	s.Messaging.Channels = nil
	for _, c := range p.GetChannels() {
		cs := v1alpha1.ChannelSpec{Name: c.GetName(), Description: c.GetDescription(), DefaultDelivery: v1alpha1.DeliveryMode(c.GetDefaultDelivery()),
			PostsPerMinute: c.GetPostsPerMinute()}
		for _, mb := range c.GetMembers() {
			cs.Members = append(cs.Members, v1alpha1.ChannelMember{Select: mb.GetSelect(), Access: v1alpha1.ChannelAccess(mb.GetAccess())})
		}
		for _, d := range c.GetAllowedDelivery() {
			cs.AllowedDelivery = append(cs.AllowedDelivery, v1alpha1.DeliveryMode(d))
		}
		s.Messaging.Channels = append(s.Messaging.Channels, cs)
	}
	if st := p.GetStorm(); st != nil {
		s.Messaging.Storm = v1alpha1.StormControl{PushPerMinute: st.GetPushPerMinute(), CoalesceBytes: st.GetCoalesceBytes(), FanoutPerMinute: st.GetFanoutPerMinute()}
		if d, err := time.ParseDuration(st.GetDigestInterval()); err == nil && st.GetDigestInterval() != "" {
			s.Messaging.Storm.DigestInterval = &metav1.Duration{Duration: d}
		}
	}
	if w := p.GetWarden(); w != nil {
		interrupt := s.Warden.InterruptOnHigh // unset defaults to true; keep it unset when that's what is asked
		if v := w.GetInterruptOnHigh(); interrupt != nil || !v {
			interrupt = &v
		}
		s.Warden = v1alpha1.WardenSpec{Mode: v1alpha1.WardenMode(w.GetMode()), Model: w.GetModel(), InterruptOnHigh: interrupt,
			QuarantineOnHigh: w.GetQuarantineOnHigh(), MonthlyUSD: w.GetMonthlyUsd()}
		return
	}
	if m := p.GetWardenMode(); m != "" {
		s.Warden.Mode = v1alpha1.WardenMode(m)
	}
	if p.GetWardenModel() != "" {
		s.Warden.Model = p.GetWardenModel()
	}
}

// channelsToPlan fills a plan's channel, storm and warden fields from a spec.
func channelsToPlan(s v1alpha1.WorkspaceSpec, p *ccv1.WorkspacePlan) {
	for _, c := range s.Messaging.Channels {
		cp := &ccv1.ChannelPlan{Name: c.Name, Description: c.Description, DefaultDelivery: string(c.DefaultDelivery), PostsPerMinute: c.PostsPerMinute}
		for _, mb := range c.Members {
			cp.Members = append(cp.Members, &ccv1.ChannelMemberPlan{Select: mb.Select, Access: string(mb.Access)})
		}
		for _, d := range c.AllowedDelivery {
			cp.AllowedDelivery = append(cp.AllowedDelivery, string(d))
		}
		p.Channels = append(p.Channels, cp)
	}
	st := &ccv1.StormPlan{PushPerMinute: s.Messaging.Storm.PushPerMinute, CoalesceBytes: s.Messaging.Storm.CoalesceBytes, FanoutPerMinute: s.Messaging.Storm.FanoutPerMinute}
	if s.Messaging.Storm.DigestInterval != nil {
		st.DigestInterval = s.Messaging.Storm.DigestInterval.Duration.String()
	}
	p.Storm = st
	p.WardenMode, p.WardenModel = string(s.Warden.Mode), s.Warden.Model
	w := s.Warden
	p.Warden = &ccv1.WardenPlan{Mode: string(w.Mode), Model: w.Model, InterruptOnHigh: w.InterruptOnHigh == nil || *w.InterruptOnHigh,
		QuarantineOnHigh: w.QuarantineOnHigh, MonthlyUsd: w.MonthlyUSD}
}
