package wizard

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
)

func TestSuggestAndRoles(t *testing.T) {
	used := map[string]bool{"athena": true}
	got := Suggest(3, used)
	if strings.Join(got, ",") != "hermes,metis,apollo" {
		t.Fatalf("Suggest = %v", got)
	}
	many := Suggest(len(Names)+2, map[string]bool{})
	if len(many) != len(Names)+2 || many[len(Names)] != "athena-2" {
		t.Fatalf("overflow names: %v", many[len(Names):])
	}
	cases := map[int]string{
		1: "implementer",
		3: "coordinator,implementer,reviewer",
		5: "coordinator,implementer,implementer,implementer,reviewer",
		7: "coordinator,implementer,implementer,implementer,reviewer,ci,qa",
	}
	for n, want := range cases {
		if got := strings.Join(DefaultRoles(n), ","); got != want {
			t.Errorf("DefaultRoles(%d) = %s", n, got)
		}
	}
}

func plan() *ccv1.WorkspacePlan {
	return &ccv1.WorkspacePlan{
		Name: "acme", DisplayName: "Acme", Org: "acme",
		Groups: []*ccv1.GroupPlan{{Name: "clean"}, {Name: "dirty"}},
		Links:  []*ccv1.LinkPlan{{From: "clean", To: "dirty", Mode: "mediated", Senders: []string{"hermes"}, Recipients: []string{"metis"}}},
		Agents: []*ccv1.AgentPlan{
			{Name: "athena", Role: "coordinator", Cli: "claude", Group: "clean", Cpu: "2", Memory: "6Gi"},
			{Name: "hermes", Role: "implementer", Cli: "ore", Group: "clean"},
		},
		NotificationChannels: []*ccv1.NotificationChannelPlan{{Kind: "webPush"}, {Kind: "slack", WebhookUrl: "https://hooks.slack/x"}},
		Owners:               []string{"alice@example.com"},
		ApprovalTimeout:      "12h",
		Internet:             true,
		RunnerLabels:         []string{"swarm"},
		ForgeNotifications:   []string{"assigned", "mentioned"},
	}
}

func TestValidate(t *testing.T) {
	if probs := Validate(plan(), map[string]bool{}); len(probs) != 0 {
		t.Fatalf("valid plan refused: %v", probs)
	}
	p := plan()
	p.Name = "Acme!"
	p.Agents = append(p.Agents, &ccv1.AgentPlan{Name: "athena", Role: "x", Cli: "vim", Group: "nope", Memory: "lots"},
		&ccv1.AgentPlan{Name: "forge", Role: "x", Cli: "claude"})
	p.ApprovalTimeout = "soon"
	p.ForgeNotifications = []string{"everything"}
	probs := Validate(p, map[string]bool{"hermes": true})
	joined := strings.Join(probs, "\n")
	for _, want := range []string{"workspace name", `"athena" appears twice`, "CLI must be", "unknown group", "not a quantity",
		`"hermes": the name is already used`, "reserved for forge", "approval timeout", "unknown forge notification"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing problem %q in:\n%s", want, joined)
		}
	}
}

func newClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(k8s.Scheme()).WithObjects(objs...).WithStatusSubresource(&v1alpha1.Workspace{}).Build()
}

func TestApplyAndReapply(t *testing.T) {
	ctx := context.Background()
	// The workspace already exists with a namespace (as the operator would set),
	// plus a field the wizard doesn't manage.
	c := newClient()
	cur, err := LoadCurrent(ctx, c, "acme")
	if err != nil || cur.Workspace != nil {
		t.Fatalf("load empty: %+v %v", cur, err)
	}
	d := Build(plan(), cur)
	changes := Diff(cur, d, false)
	if changes[0].Action != ccv1.ChangeAction_CHANGE_ACTION_CREATE || len(changes) != 4 {
		t.Fatalf("first diff: %+v", changes)
	}
	if d.Workspace.Spec.Notifications.Channels[1].SecretName != "notify-acme-slack" {
		t.Fatalf("channel secret %+v", d.Workspace.Spec.Notifications.Channels)
	}
	// Apply before the namespace exists: workspace created, rooms wait.
	if err := Apply(ctx, c, cur, d, changes, 0); err != ErrNamespaceNotReady {
		t.Fatalf("apply without namespace: %v", err)
	}
	var ws v1alpha1.Workspace
	if err := c.Get(ctx, types.NamespacedName{Name: "acme"}, &ws); err != nil {
		t.Fatal(err)
	}
	ws.Status.Namespace = "ws-acme"
	if err := c.Status().Update(ctx, &ws); err != nil {
		t.Fatal(err)
	}
	cur, _ = LoadCurrent(ctx, c, "acme")
	d = Build(plan(), cur)
	changes = Diff(cur, d, false)
	if changes[0].Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
		t.Fatalf("workspace should be unchanged: %+v", changes[0])
	}
	if err := Apply(ctx, c, cur, d, changes, time.Second); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var rooms v1alpha1.RoomList
	c.List(ctx, &rooms, client.InNamespace("ws-acme"))
	if len(rooms.Items) != 2 {
		t.Fatalf("rooms %d", len(rooms.Items))
	}

	// Someone sets a field the wizard doesn't show; re-applying keeps it and
	// touches only what changed.
	var athena v1alpha1.Room
	c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "athena"}, &athena)
	athena.Spec.Env = map[string]string{"FOO": "bar"}
	c.Update(ctx, &athena)

	p := plan()
	p.Agents[1].Model = "gpt-6"
	p.Agents = p.Agents[1:] // drop athena from the plan
	cur, _ = LoadCurrent(ctx, c, "acme")
	d = Build(p, cur)
	changes = Diff(cur, d, false)
	acts := map[string]ccv1.ChangeAction{}
	for _, ch := range changes {
		acts[ch.Kind+"/"+ch.Name] = ch.Action
	}
	if acts["Room/hermes"] != ccv1.ChangeAction_CHANGE_ACTION_UPDATE || acts["Workspace/acme"] != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
		t.Fatalf("re-apply diff %v", acts)
	}
	if _, ok := acts["Room/athena"]; ok {
		t.Fatal("athena listed without prune")
	}
	changes = Diff(cur, d, true)
	for _, ch := range changes {
		if ch.Name == "athena" && ch.Action != ccv1.ChangeAction_CHANGE_ACTION_DELETE {
			t.Fatalf("prune: %+v", ch)
		}
		if ch.Name == "hermes" && (len(ch.Fields) != 1 || ch.Fields[0] != "model") {
			t.Fatalf("hermes fields %v", ch.Fields)
		}
	}
	if err := Apply(ctx, c, cur, d, changes, time.Second); err != nil {
		t.Fatal(err)
	}
	c.List(ctx, &rooms, client.InNamespace("ws-acme"))
	if len(rooms.Items) != 1 || rooms.Items[0].Spec.Model != "gpt-6" {
		t.Fatalf("after prune: %+v", rooms.Items)
	}

	// The plan read back from the cluster round-trips.
	cur, _ = LoadCurrent(ctx, c, "acme")
	got := FromCluster(cur)
	if got.Org != "acme" || len(got.Agents) != 1 || got.ApprovalTimeout != "12h0m0s" || len(got.RunnerLabels) != 1 || len(got.ForgeNotifications) != 2 {
		t.Fatalf("FromCluster %+v", got)
	}
	again := Build(got, cur)
	for _, ch := range Diff(cur, again, true) {
		if ch.Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			t.Fatalf("round trip changes %s/%s %v", ch.Kind, ch.Name, ch.Fields)
		}
	}
	_ = resource.MustParse
}
