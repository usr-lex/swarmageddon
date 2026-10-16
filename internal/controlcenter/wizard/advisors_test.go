package wizard

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

func TestAdvisorsAndWarden(t *testing.T) {
	ctx := context.Background()
	ws := &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: "acme"}}}
	ws.Status.Namespace = "ws-acme"
	impl := &v1alpha1.RoleTemplate{ObjectMeta: metav1.ObjectMeta{Name: "implementer", Namespace: "ws-acme"},
		Spec: v1alpha1.RoleTemplateSpec{Advisor: &v1alpha1.AdvisorSpec{Model: "old"}}}
	coord := &v1alpha1.RoleTemplate{ObjectMeta: metav1.ObjectMeta{Name: "coordinator", Namespace: "ws-acme"}}
	c := newClient(ws, impl, coord)

	p := plan()
	p.RoleAdvisors = map[string]*ccv1.AdvisorPlan{
		"coordinator": {Model: "claude-opus-5-5", Effort: "max", MaxCallsPerHour: 5, MonthlyUsd: 30, Native: true},
		"implementer": {Model: ""}, // removes the template's advisor
	}
	p.Agents[0].Advisor = &ccv1.AdvisorPlan{Model: ""} // athena: no advisor, whatever the role says
	p.Agents[0].WardenMode = "gate"
	p.Agents[1].Advisor = &ccv1.AdvisorPlan{Model: "gpt-6", Effort: "high"}
	p.Warden = &ccv1.WardenPlan{Mode: "gate", Model: "haiku", InterruptOnHigh: false, QuarantineOnHigh: true, MonthlyUsd: 15}
	if probs := Validate(p, nil); len(probs) != 0 {
		t.Fatalf("valid plan refused: %v", probs)
	}

	cur, err := LoadCurrent(ctx, c, "acme")
	if err != nil {
		t.Fatal(err)
	}
	d := Build(p, cur)
	w := d.Workspace.Spec.Warden
	if w.Mode != v1alpha1.WardenGate || w.Model != "haiku" || w.InterruptOnHigh == nil || *w.InterruptOnHigh || !w.QuarantineOnHigh || w.MonthlyUSD != 15 {
		t.Fatalf("warden %+v", w)
	}
	athena, hermes := d.Rooms[0].Spec, d.Rooms[1].Spec
	if athena.Advisor == nil || athena.Advisor.Model != "" || athena.WardenMode != v1alpha1.WardenGate {
		t.Fatalf("athena %+v %q", athena.Advisor, athena.WardenMode)
	}
	if hermes.Advisor == nil || hermes.Advisor.Model != "gpt-6" || hermes.WardenMode != "" {
		t.Fatalf("hermes %+v", hermes.Advisor)
	}
	var fields []string
	for _, ch := range Diff(cur, d, false) {
		if ch.Kind == "RoleTemplate" {
			fields = append(fields, ch.Name+":"+strings.Join(ch.Fields, ","))
		}
	}
	slices.Sort(fields)
	if strings.Join(fields, " ") != "coordinator:advisor implementer:advisor" {
		t.Fatalf("template changes %v", fields)
	}
	if err := Apply(ctx, c, cur, d, Diff(cur, d, false), time.Second); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.RoleTemplate
	c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "coordinator"}, &got)
	if a := got.Spec.Advisor; a == nil || a.Model != "claude-opus-5-5" || a.Effort != "max" || a.MaxCallsPerHour != 5 || a.MonthlyUSD != 30 || !a.Native {
		t.Fatalf("coordinator advisor %+v", got.Spec.Advisor)
	}
	c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "implementer"}, &got)
	if got.Spec.Advisor != nil {
		t.Fatalf("implementer advisor kept: %+v", got.Spec.Advisor)
	}

	// Read back and rebuilt, nothing changes.
	cur, _ = LoadCurrent(ctx, c, "acme")
	back := FromCluster(cur)
	if back.Warden.GetModel() != "haiku" || back.Warden.GetInterruptOnHigh() || back.RoleAdvisors["coordinator"].GetModel() != "claude-opus-5-5" ||
		back.Agents[0].GetAdvisor() == nil || back.Agents[0].GetWardenMode() != "gate" {
		t.Fatalf("FromCluster %+v", back)
	}
	for _, ch := range Diff(cur, Build(back, cur), true) {
		if ch.Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			t.Fatalf("round trip changes %s/%s %v", ch.Kind, ch.Name, ch.Fields)
		}
	}

	// Bad values are refused.
	p.Agents[1].WardenMode = "paranoid"
	p.RoleAdvisors["qa"] = &ccv1.AdvisorPlan{Model: "x", MaxCallsPerHour: -1}
	p.Warden.Mode = "maybe"
	probs := strings.Join(Validate(p, nil), "\n")
	for _, want := range []string{`agent "hermes": the warden mode`, "role qa: advisor limits", "the warden mode must be"} {
		if !strings.Contains(probs, want) {
			t.Errorf("missing %q in:\n%s", want, probs)
		}
	}
}
