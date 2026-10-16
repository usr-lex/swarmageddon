package workspace

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/pki"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

func names(chs []v1alpha1.ChannelSpec) string {
	var out []string
	for _, c := range chs {
		out = append(out, c.Name)
	}
	return strings.Join(out, ",")
}

func TestDefaultChannels(t *testing.T) {
	ws := &directory.Workspace{
		Name:  "acme",
		Roles: map[string]bool{"coordinator": true, "reviewer": true, "qa": true},
		Rooms: map[string]directory.Room{
			"hermes": {Name: "hermes", Group: "clean", Role: "coordinator"},
			"athena": {Name: "athena", Group: "clean", Role: "reviewer"},
			"apollo": {Name: "apollo", Group: "clean", Role: "reviewer"},
			"metis":  {Name: "metis", Group: "dirty", Role: "reviewer"},
			"ares":   {Name: "ares", Group: "dirty", Role: "qa"},
		},
	}
	open := []v1alpha1.GroupLink{{From: "clean", To: "dirty", Mode: v1alpha1.LinkOpen}, {From: "dirty", To: "clean", Mode: v1alpha1.LinkOpen}}
	ws.Messaging.Links = open
	got := DefaultChannels(ws)
	if names(got) != "announcements,general,reviewers" {
		t.Fatalf("open workspace channels %s", names(got))
	}
	ws.Messaging.Channels = got
	if problems, _ := directory.ValidateChannels(ws); len(problems) > 0 {
		t.Fatalf("defaults invalid: %v", problems)
	}

	// Walled: a set per group; dirty has only one reviewer.
	ws.Messaging.Links = []v1alpha1.GroupLink{{From: "clean", To: "dirty", Mode: v1alpha1.LinkMediated}, open[1]}
	got = DefaultChannels(ws)
	if names(got) != "announcements-clean,general-clean,reviewers-clean,announcements-dirty,general-dirty" {
		t.Fatalf("walled channels %s", names(got))
	}
	ws.Messaging.Channels = got
	if problems, _ := directory.ValidateChannels(ws); len(problems) > 0 {
		t.Fatalf("walled defaults invalid: %v", problems)
	}
}

func TestChannelsCondition(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ca, err := pki.NewCA("test", metav1.Now().Time)
	must(err)
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme, Deps: deps.Deps{PKI: ca}}
	must(c.Create(ctx, &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "chan"}, Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: "chan"}}}))
	reconcile := func() *v1alpha1.Workspace {
		t.Helper()
		_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "chan"}})
		var w v1alpha1.Workspace
		must(c.Get(ctx, types.NamespacedName{Name: "chan"}, &w))
		return &w
	}
	w := reconcile()
	if len(w.Spec.Messaging.Channels) != 0 {
		t.Fatal("defaults before any room")
	}
	for _, rm := range []struct{ name, role string }{{"hermes", "coordinator"}, {"athena", "reviewer"}, {"apollo", "reviewer"}} {
		must(c.Create(ctx, &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-chan", Name: rm.name},
			Spec: v1alpha1.RoomSpec{Role: rm.role, CLI: v1alpha1.CLIClaude}}))
	}
	w = reconcile()
	if names(w.Spec.Messaging.Channels) != "announcements,general,reviewers" || w.Annotations[AnnotationDefaultChannels] == "" {
		t.Fatalf("defaults %s %v", names(w.Spec.Messaging.Channels), w.Annotations)
	}
	if cnd := meta.FindStatusCondition(w.Status.Conditions, ConditionChannelsValid); cnd == nil || cnd.Status != metav1.ConditionTrue {
		t.Fatalf("condition %+v", cnd)
	}
	// Removing them all doesn't bring them back; a bad channel is reported.
	w.Spec.Messaging.Channels = []v1alpha1.ChannelSpec{
		{Name: "ops", Members: []v1alpha1.ChannelMember{{Select: "group:nowhere", Access: v1alpha1.AccessRead}}},
		{Name: "ops", Members: []v1alpha1.ChannelMember{{Select: "everyone", Access: v1alpha1.AccessRead}}},
	}
	must(c.Update(ctx, w))
	w = reconcile()
	cnd := meta.FindStatusCondition(w.Status.Conditions, ConditionChannelsValid)
	if cnd == nil || cnd.Status != metav1.ConditionFalse || !strings.Contains(cnd.Message, "#ops is defined twice") ||
		!strings.Contains(cnd.Message, "no group nowhere") {
		t.Fatalf("condition %+v", cnd)
	}
	w.Spec.Messaging.Channels = nil
	must(c.Update(ctx, w))
	if w = reconcile(); len(w.Spec.Messaging.Channels) != 0 {
		t.Fatal("defaults came back")
	}
}
