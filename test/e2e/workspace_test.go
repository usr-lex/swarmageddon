//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

// agent is one planned room.
func agent(name, role, cli, group string) *ccv1.AgentPlan {
	model := "mock-claude"
	if cli == "ore" {
		model = "mock-ore"
	}
	// Small rooms, so six fit on a CI runner next to the platform.
	return &ccv1.AgentPlan{Name: name, Role: role, Cli: cli, Model: model, Group: group,
		Cpu: "300m", Memory: "1Gi", HomeVolume: "2Gi", EngineVolume: "2Gi", Repos: []string{ws + "/api"}}
}

// rooms of the plan: both CLIs, a coordinator, implementers and a reviewer
// in group clean, and one implementer behind a wall in group dirty.
var planned = []string{"hermes", "athena", "apollo", "artemis", "metis"}

// plan is the workspace the wizard writes: two groups joined only by
// mediated links (hermes ↔ metis), one repository and a runner pool.
func plan(extra ...*ccv1.AgentPlan) *ccv1.WorkspacePlan {
	return &ccv1.WorkspacePlan{
		Name: ws, DisplayName: "Acme (e2e)", Org: ws, Owners: []string{"admin@example.com"},
		Groups:       []*ccv1.GroupPlan{{Name: "clean"}, {Name: "dirty"}},
		DefaultLinks: "withinGroup",
		Links: []*ccv1.LinkPlan{
			{From: "clean", To: "dirty", Mode: "mediated", Senders: []string{"hermes"}, Recipients: []string{"metis"}},
			{From: "dirty", To: "clean", Mode: "mediated", Senders: []string{"metis"}, Recipients: []string{"hermes"}},
		},
		Agents: append([]*ccv1.AgentPlan{
			agent("hermes", "coordinator", "ore", "clean"),
			agent("athena", "implementer", "claude", "clean"),
			agent("apollo", "implementer", "ore", "clean"),
			agent("artemis", "reviewer", "claude", "clean"),
			agent("metis", "implementer", "claude", "dirty"),
		}, extra...),
		Repositories:            []*ccv1.RepositoryPlan{{Name: "api"}},
		DefaultModel:            "mock-claude",
		BudgetPerRoomMonthlyUsd: 10,
		RequiredApprovals:       1,
		RunnerLabels:            []string{runner},
		RunnerMaxConcurrent:     2,
	}
}

// apply is the wizard's Apply button: it retries while the new workspace's
// namespace isn't there yet, as the UI does.
func (s *suite) apply(t *testing.T, p *ccv1.WorkspacePlan, prune bool) *ccv1.ApplyWorkspaceResponse {
	t.Helper()
	var res *ccv1.ApplyWorkspaceResponse
	s.eventually(t, 5*time.Minute, "ApplyWorkspace", func() error {
		var err error
		res, err = call(s.cc.workspaces.ApplyWorkspace, &ccv1.ApplyWorkspaceRequest{Plan: p, PruneRooms: prune})
		if connect.CodeOf(err) == connect.CodeUnavailable {
			return err
		}
		if err != nil {
			t.Fatalf("ApplyWorkspace: %v", err)
		}
		return nil
	})
	return res
}

// workspace runs the workspace wizard through its API: suggestions,
// templates, default channels, preview, apply, and a second preview that
// must change nothing (§8.3).
func (s *suite) workspace(t *testing.T) {
	sug, err := call(s.cc.workspaces.SuggestAgentNames, &ccv1.SuggestAgentNamesRequest{Count: 5})
	if err != nil {
		t.Fatal(err)
	}
	if r := sug.GetRoles(); len(sug.GetNames()) != 5 || len(r) != 5 || r[0] != "coordinator" || r[4] != "reviewer" {
		t.Errorf("SuggestAgentNames(5): %v %v; want five names, a coordinator first and a reviewer last", sug.GetNames(), r)
	}
	tpl, err := call(s.cc.workspaces.ListRoleTemplates, &ccv1.ListRoleTemplatesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"coordinator", "implementer", "reviewer"} {
		if !slices.ContainsFunc(tpl.GetTemplates(), func(x *ccv1.RoleTemplateSummary) bool { return x.GetName() == want }) {
			t.Errorf("ListRoleTemplates: no %s", want)
		}
	}
	ch, err := call(s.cc.workspaces.DefaultChannels, &ccv1.DefaultChannelsRequest{Plan: plan()})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(ch.GetChannels(), func(c *ccv1.ChannelPlan) bool { return c.GetName() == "general-clean" }) {
		t.Errorf("DefaultChannels of a walled workspace: no #general-clean in %v", ch.GetChannels())
	}

	pv, err := call(s.cc.workspaces.PreviewWorkspace, &ccv1.PreviewWorkspaceRequest{Plan: plan()})
	if err != nil {
		t.Fatalf("PreviewWorkspace: %v", err)
	}
	if len(pv.GetProblems()) > 0 {
		t.Fatalf("PreviewWorkspace problems: %v", pv.GetProblems())
	}
	t.Logf("preview: fits %v (%s)", pv.GetFits(), pv.GetFitNote())
	for _, name := range planned {
		if c := change(pv.GetChanges(), "Room", name); c == nil {
			t.Errorf("PreviewWorkspace: no change listed for room %s", name)
		}
	}
	s.apply(t, plan(), false)

	s.eventually(t, 10*time.Minute, "the workspace's namespace, templates, forge org, repository and webhook", func() error {
		gw, err := call(s.cc.workspaces.GetWorkspace, &ccv1.GetWorkspaceRequest{Name: ws})
		if err != nil {
			return err
		}
		return notTrue(gw.GetSummary().GetConditions(), "NamespaceReady", "RoleTemplatesReady", "ForgeOrgReady",
			"RepositoriesReady", "ForgeWebhookReady", "GatewayTeamReady")
	})
	again, err := call(s.cc.workspaces.PreviewWorkspace, &ccv1.PreviewWorkspaceRequest{Plan: plan()})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range again.GetChanges() {
		if c.GetAction() != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			t.Errorf("re-running the wizard with the same plan would %v %s/%s (fields %v)", c.GetAction(), c.GetKind(), c.GetName(), c.GetFields())
		}
	}
}

func change(cs []*ccv1.Change, kind, name string) *ccv1.Change {
	for _, c := range cs {
		if c.GetKind() == kind && c.GetName() == name {
			return c
		}
	}
	return nil
}

// roomsRun waits for every planned room to run, with its supervisor up and
// reporting presence (fleet view, §8.4).
func (s *suite) roomsRun(t *testing.T) {
	s.waitRooms(t, planned...)
	for _, r := range planned {
		out, err := s.inRoom(r, "swarm whoami")
		if err != nil || !strings.Contains(out, r+"@"+ws) {
			t.Errorf("%s: swarm whoami: %v: %s", r, err, clip(out))
		}
	}
}

// waitRooms waits until the rooms run and report presence.
func (s *suite) waitRooms(t *testing.T, names ...string) {
	t.Helper()
	s.eventually(t, 20*time.Minute, "rooms running", func() error {
		lr, err := call(s.cc.rooms.ListRooms, &ccv1.ListRoomsRequest{Workspace: ws})
		if err != nil {
			return err
		}
		var bad []string
		for _, name := range names {
			i := slices.IndexFunc(lr.GetRooms(), func(r *ccv1.RoomSummary) bool { return r.GetName() == name })
			if i < 0 {
				bad = append(bad, name+": not listed")
				continue
			}
			r := lr.GetRooms()[i]
			if r.GetPhase() != "Running" {
				var why []string
				for _, c := range r.GetConditions() {
					if c.GetStatus() != "True" && c.GetType() != "Quarantined" {
						why = append(why, c.GetType()+": "+c.GetMessage())
					}
				}
				bad = append(bad, fmt.Sprintf("%s: %s (%s)", name, r.GetPhase(), strings.Join(why, "; ")))
				continue
			}
			if p := r.GetPresence(); p == ccv1.Presence_PRESENCE_UNSPECIFIED || p == ccv1.Presence_PRESENCE_OFFLINE {
				bad = append(bad, fmt.Sprintf("%s: running, presence %v", name, p))
			}
		}
		if len(bad) > 0 {
			return errors.New(strings.Join(bad, "\n"))
		}
		return nil
	})
}
