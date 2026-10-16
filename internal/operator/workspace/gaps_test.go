package workspace

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/room"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// The room controller leaves only teams it considers managed; they must be
// exactly the teams this controller creates.
func TestManagedTeamsMatch(t *testing.T) {
	w := &v1alpha1.Workspace{Spec: v1alpha1.WorkspaceSpec{
		Groups: []v1alpha1.Group{{Name: "dirty"}},
		Forge:  v1alpha1.ForgeSpec{Teams: []v1alpha1.TeamSpec{{Name: "leads"}}},
	}}
	for _, tm := range teams(w) {
		if !room.IsManagedTeam(w, tm.Name) {
			t.Errorf("team %s is created here but not managed by the room controller", tm.Name)
		}
	}
	for _, n := range room.BuiltinTeams {
		if !slices.ContainsFunc(teams(w), func(t forgejo.Team) bool { return t.Name == n }) {
			t.Errorf("built-in %s not created", n)
		}
	}
	if room.IsManagedTeam(w, "people") {
		t.Error("a hand-made team is managed")
	}
}

func TestSharedVolumesAndRepoCredentials(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SystemNamespace}}))
	forge, forgeSrv := newFakeForge(t)
	ca, err := pki.NewCA("test", metav1.Now().Time)
	must(err)
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme, Deps: deps.Deps{
		PKI: ca, Version: "v0.1.0",
		Forge: func(context.Context) (*forgejo.Client, error) { return forgejo.New(forgeSrv.URL, "admin"), nil },
	}}
	w := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: v1alpha1.WorkspaceSpec{
			Forge:  v1alpha1.ForgeSpec{Org: "acme"},
			Groups: []v1alpha1.Group{{Name: "clean"}, {Name: "dirty"}},
			Messaging: v1alpha1.MessagingPolicy{Links: []v1alpha1.GroupLink{
				{From: "clean", To: "dirty", Mode: v1alpha1.LinkMediated},
			}},
			SharedVolumes: []v1alpha1.SharedVolume{{Name: "specs", Size: resource.MustParse("1Gi"), Mounts: []v1alpha1.VolumeMount{
				{Select: "group:clean"}, {Select: "group:dirty"},
			}}},
			Repositories: []v1alpha1.RepositorySpec{{Name: "api", ImportURL: "https://github.com/acme/api.git", ImportSecretName: "github",
				PushMirror: "https://github.com/acme/api-mirror.git", MirrorSecretName: "github"}},
		},
	}
	must(c.Create(ctx, w))
	reconcile := func() *v1alpha1.Workspace {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}}); err != nil {
			t.Fatal(err)
		}
		var got v1alpha1.Workspace
		must(c.Get(ctx, types.NamespacedName{Name: "acme"}, &got))
		return &got
	}
	got := reconcile()
	// The credential Secret isn't there yet: the repository waits.
	if cnd := meta.FindStatusCondition(got.Status.Conditions, ConditionRepositoriesReady); cnd == nil || cnd.Status != metav1.ConditionFalse ||
		!strings.Contains(cnd.Message, "ws-acme/github") {
		t.Errorf("repositories condition = %+v", cnd)
	}
	if forge.count("POST /repos/migrate") != 0 {
		t.Error("imported without its credential")
	}
	if cnd := meta.FindStatusCondition(got.Status.Conditions, ConditionSharedVolumesValid); cnd == nil || cnd.Status != metav1.ConditionTrue {
		t.Errorf("shared volumes condition with no rooms = %+v", cnd)
	}

	// The credential lives in the workspace namespace.
	must(c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "ws-acme"},
		StringData: map[string]string{"token": "ghp_x"}}))
	for _, rm := range []struct{ name, group string }{{"athena", "clean"}, {"metis", "dirty"}} {
		must(c.Create(ctx, &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: rm.name, Namespace: "ws-acme"},
			Spec: v1alpha1.RoomSpec{Role: "reviewer", CLI: v1alpha1.CLIClaude, Group: rm.group}}))
	}
	got = reconcile()
	if b := forge.bodies["POST /repos/migrate"]; b == nil || b["auth_token"] != "ghp_x" || b["auth_username"] != "oauth2" {
		t.Errorf("migrate body = %v", b)
	}
	if b := forge.bodies["POST /repos/acme/api/push_mirrors"]; b == nil || b["remote_password"] != "ghp_x" {
		t.Errorf("push mirror body = %v", b)
	}
	cnd := meta.FindStatusCondition(got.Status.Conditions, ConditionSharedVolumesValid)
	if cnd == nil || cnd.Status != metav1.ConditionFalse || !strings.Contains(cnd.Message, "specs") {
		t.Errorf("shared volumes condition = %+v", cnd)
	}
}
