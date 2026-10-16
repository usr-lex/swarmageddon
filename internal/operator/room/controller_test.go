package room

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/room/roomtest"
)

const ns = "ws-acme"

type env struct {
	t      *testing.T
	c      client.Client
	real   bool
	r      *Reconciler
	forge  *roomtest.Forge
	gw     *roomtest.Gateway
	images *roomtest.Images
	now    time.Time
}

type idleFunc func() bool

func (f idleFunc) Idle(context.Context, string, string) (bool, error) { return f(), nil }

func newEnv(t *testing.T) *env {
	c, op, real := roomtest.Client(t)
	e := &env{t: t, c: c, real: real, forge: roomtest.NewForge(t, "reviewers", "implementers", "mergers", "group-clean"),
		gw: roomtest.NewGateway(t), images: &roomtest.Images{RoomImage: "room:1", RunnerImage: "runner:1"},
		now: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	e.r = &Reconciler{Client: op, Idle: NeverIdle{}, Now: func() time.Time { return e.now }, Deps: deps.Deps{
		PKI: roomtest.NewPKI(), Images: e.images, Forge: e.forge.Client, Gateway: e.gw.Client,
		PlatformTier: func(context.Context) (string, error) { return "userns", nil },
	}}
	ctx := context.Background()
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	must(t, c.Create(ctx, &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1.WorkspaceSpec{
		Forge:  v1.ForgeSpec{Org: "acme", Teams: []v1.TeamSpec{{Name: "mergers", Members: []string{"athena"}}}},
		Models: v1.ModelSettings{Default: "claude-opus-5-5", Budgets: v1.BudgetSettings{PerRoomMonthlyUSD: 200}},
	}}))
	must(t, c.Create(ctx, &v1.RoleTemplate{ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: ns}, Spec: v1.RoleTemplateSpec{
		Title: "Reviewer", RoleText: "Review.", ForgeTeams: []string{"reviewers"},
	}}))
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) addRoom(name string, mut func(*v1.Room)) {
	r := &v1.Room{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1.RoomSpec{Role: "reviewer", CLI: v1.CLIClaude}}
	if mut != nil {
		mut(r)
	}
	must(e.t, e.c.Create(context.Background(), r))
}

func (e *env) reconcile(names ...string) {
	e.t.Helper()
	for range 3 {
		for _, n := range names {
			if _, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: n}}); err != nil {
				e.t.Fatalf("reconcile %s: %v", n, err)
			}
		}
	}
}

func (e *env) room(name string) *v1.Room {
	var r v1.Room
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &r))
	return &r
}

func (e *env) sts(name string) *appsv1.StatefulSet {
	var s appsv1.StatefulSet
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: Name(name)}, &s))
	return &s
}

func (e *env) secret(name string) *corev1.Secret {
	var s corev1.Secret
	must(e.t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: IdentitySecretName(name)}, &s))
	return &s
}

func (e *env) update(name string, mut func(*v1.Room)) {
	r := e.room(name)
	mut(r)
	must(e.t, e.c.Update(context.Background(), r))
}

type snapshot struct {
	gen  int64
	rv   string
	hash string
	uid  types.UID
}

func (e *env) snap(name string) snapshot {
	s := e.sts(name)
	return snapshot{s.Generation, s.ResourceVersion, TemplateHash(&s.Spec.Template), s.UID}
}

func TestProvisionsRoom(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", func(r *v1.Room) { r.Spec.Group = "clean" })
	e.reconcile("athena")

	r := e.room("athena")
	for _, c := range []string{v1.ConditionForgeAccountReady, v1.ConditionKeysReady, v1.ConditionGatewayKeyReady, v1.ConditionMessagingIdentityReady} {
		if !meta.IsStatusConditionTrue(r.Status.Conditions, c) {
			t.Errorf("condition %s not true: %+v", c, meta.FindStatusCondition(r.Status.Conditions, c))
		}
	}
	if r.Status.ForgeUser != "athena" || r.Status.Address != "athena@acme" || r.Status.ImageVersion != "room:1" {
		t.Errorf("status = %+v", r.Status)
	}
	sec := e.secret("athena")
	for _, k := range []string{KeySSHPrivate, KeySSHPublic, KeyGPGSecret, KeyForgeToken, KeyTLSCert, KeyTLSKey, KeyCA, KeyGatewayKey, KeyVNCPassword} {
		if len(sec.Data[k]) == 0 {
			t.Errorf("secret key %s empty", k)
		}
	}
	if _, ok := sec.Data[KeyForgeKnownHost]; !ok {
		t.Error("forge_known_hosts key missing")
	}

	// Forge account: user, restrictions, keys, verified GPG via Sudo, token, teams.
	if len(e.forge.Calls("POST", "/admin/users")) == 0 {
		t.Error("forge user not created")
	}
	if c := e.forge.Calls("POST", "/user/gpg_keys"); len(c) != 1 || c[0].Sudo != "athena" || c[0].Body["armored_signature"] == "" {
		t.Errorf("gpg calls = %+v", c)
	}
	if c := e.forge.Calls("POST", "/admin/users/athena/tokens"); len(c) != 1 {
		t.Errorf("token calls = %+v", c)
	}
	for _, team := range []string{"/teams/1/members/athena", "/teams/3/members/athena", "/teams/4/members/athena"} {
		if len(e.forge.Calls("PUT", team)) == 0 {
			t.Errorf("not added to %s", team)
		}
	}
	// Stable: no further forge provisioning once synced.
	before := len(e.forge.Calls("PATCH", "/admin/users/athena"))
	e.reconcile("athena")
	if after := len(e.forge.Calls("PATCH", "/admin/users/athena")); after != before {
		t.Errorf("forge re-provisioned on a no-op reconcile (%d -> %d)", before, after)
	}
	// Gateway key with the workspace budget and metadata.
	g := e.gw.Calls("POST", "/key/generate")
	if len(g) != 1 || g[0].Body["max_budget"] != 200.0 || g[0].Body["metadata"].(map[string]any)["room"] != "athena" {
		t.Errorf("gateway calls = %+v", g)
	}

	s := e.sts("athena")
	ps := s.Spec.Template.Spec
	if *s.Spec.Replicas != 1 || ps.Hostname != "athena" || *ps.AutomountServiceAccountToken || *ps.EnableServiceLinks || ps.HostUsers == nil || *ps.HostUsers {
		t.Errorf("pod spec = %+v", ps)
	}
	if s.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Error("PVCs not retained")
	}
	c := ps.Containers[0]
	if c.Resources.Requests.Memory().Cmp(*c.Resources.Limits.Memory()) != 0 || *c.SecurityContext.ProcMount != corev1.UnmaskedProcMount {
		t.Errorf("container = %+v", c)
	}
	var sa corev1.ServiceAccount
	must(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "room-athena"}, &sa))
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("service account automounts its token")
	}
	var cm corev1.ConfigMap
	must(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: PersonaConfigMapName("athena")}, &cm))
	if !strings.Contains(cm.Data["room.json"], `"room": "athena"`) || cm.Data["claude-managed-settings.json"] == "" {
		t.Errorf("persona = %v", cm.Data)
	}
	var np networkingv1.NetworkPolicy
	must(t, e.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "room-athena"}, &np))
	if len(np.Spec.Egress) != 6 { // dns, 4 platform services, internet
		t.Errorf("egress rules = %d", len(np.Spec.Egress))
	}
}

// One room never restarts another (design §5).
func TestRoomIsolation(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", nil)
	e.addRoom("hermes", nil)
	e.reconcile("athena", "hermes")
	a := e.snap("athena")
	check := func(step string) {
		t.Helper()
		e.reconcile("athena", "hermes")
		if got := e.snap("athena"); got != a {
			t.Fatalf("%s changed room A's StatefulSet: %+v -> %+v", step, a, got)
		}
	}

	e.update("hermes", func(r *v1.Room) {
		r.Spec.Resources.Memory = ptr.To(resource.MustParse("12Gi"))
		r.Spec.Env = map[string]string{"X": "y"}
	})
	check("changing B")
	if e.snap("hermes").hash == a.hash {
		t.Fatal("B's template did not change")
	}
	e.update("hermes", func(r *v1.Room) { r.Spec.Paused = true })
	check("pausing B")
	if *e.sts("hermes").Spec.Replicas != 0 {
		t.Error("B not scaled to 0")
	}
	e.update("hermes", func(r *v1.Room) {
		r.Annotations = map[string]string{v1.AnnotationRotate: "1", v1.AnnotationRecreate: "1"}
	})
	check("rotating and recreating B")
	e.addRoom("metis", nil)
	e.reconcile("metis")
	check("adding C")
	var ws v1.Workspace
	must(t, e.c.Get(context.Background(), types.NamespacedName{Name: "acme"}, &ws))
	ws.Spec.Models.Budgets.PerRoomMonthlyUSD = 500
	ws.Spec.Internet = ptr.To(false)
	must(t, e.c.Update(context.Background(), &ws))
	check("changing the workspace")
	must(t, e.c.Delete(context.Background(), e.room("hermes")))
	e.reconcile("hermes")
	check("deleting B")
}

func TestRotate(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", nil)
	e.reconcile("athena")
	old := e.secret("athena")
	a := e.snap("athena")

	e.update("athena", func(r *v1.Room) { r.Annotations = map[string]string{v1.AnnotationRotate: "2026-10-16"} })
	e.reconcile("athena")
	sec := e.secret("athena")
	for _, k := range []string{KeySSHPrivate, KeyGPGSecret, KeyForgeToken, KeyGatewayKey, KeyTLSCert, KeyVNCPassword} {
		if string(sec.Data[k]) == string(old.Data[k]) {
			t.Errorf("%s not rotated", k)
		}
	}
	if len(e.gw.Calls("POST", "/key/delete")) != 1 {
		t.Error("old gateway key not revoked")
	}
	if len(e.forge.Calls("GET", "/admin/users/athena/tokens")) == 0 {
		t.Error("old forge tokens not revoked")
	}
	if e.room("athena").Status.CredentialsRotatedAt == nil {
		t.Error("credentialsRotatedAt not set")
	}
	if got := e.snap("athena"); got.hash != a.hash || (e.real && got.gen != a.gen) {
		t.Error("rotation restarted the room")
	}
	// Handled once.
	n := len(e.gw.Calls("POST", "/key/generate"))
	e.reconcile("athena")
	if len(e.gw.Calls("POST", "/key/generate")) != n {
		t.Error("rotated again")
	}
}

func TestImageUpdatePolicy(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", nil)
	e.addRoom("hermes", nil)
	e.reconcile("athena", "hermes")

	e.images.RoomImage = "room:2"
	e.reconcile("athena", "hermes")
	if img := e.sts("athena").Spec.Template.Spec.Containers[0].Image; img != "room:1" {
		t.Fatalf("rolled without permission: %s", img)
	}
	if !e.room("athena").Status.UpdateAvailable {
		t.Error("updateAvailable not set")
	}

	e.update("athena", func(r *v1.Room) { r.Annotations = map[string]string{v1.AnnotationUpdateNow: "1"} })
	e.reconcile("athena", "hermes")
	if img := e.sts("athena").Spec.Template.Spec.Containers[0].Image; img != "room:2" {
		t.Fatalf("update-now ignored: %s", img)
	}
	if e.room("athena").Status.UpdateAvailable {
		t.Error("updateAvailable still set")
	}
	if img := e.sts("hermes").Spec.Template.Spec.Containers[0].Image; img != "room:1" {
		t.Error("update-now on A rolled B")
	}

	// Idle rooms roll by themselves, one at a time per workspace.
	e.r.Idle = idleFunc(func() bool { return true })
	e.now = e.now.Add(time.Second)
	e.reconcile("hermes")
	if img := e.sts("hermes").Spec.Template.Spec.Containers[0].Image; img != "room:1" {
		t.Error("rolled right after another room")
	}
	e.now = e.now.Add(5 * time.Minute)
	e.reconcile("hermes")
	if img := e.sts("hermes").Spec.Template.Spec.Containers[0].Image; img != "room:2" {
		t.Error("idle room not rolled")
	}
}

func TestDeletionRetainsVolumes(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", nil)
	e.reconcile("athena")
	ctx := context.Background()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "home-room-athena-0", Namespace: ns, Labels: SelectorLabels("athena")},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}},
	}
	must(t, e.c.Create(ctx, pvc))

	must(t, e.c.Delete(ctx, e.room("athena")))
	e.reconcile("athena")
	if err := e.c.Get(ctx, client.ObjectKeyFromObject(&v1.Room{ObjectMeta: metav1.ObjectMeta{Name: "athena", Namespace: ns}}), &v1.Room{}); !apierrors.IsNotFound(err) {
		t.Fatalf("room not deleted: %v", err)
	}
	if len(e.forge.Calls("PATCH", "/admin/users/athena")) == 0 {
		t.Error("forge user not disabled")
	}
	last := e.forge.Calls("PATCH", "/admin/users/athena")
	if b := last[len(last)-1].Body; b["active"] != false {
		t.Errorf("last user patch = %v", b)
	}
	if len(e.gw.Calls("POST", "/key/delete")) != 1 {
		t.Error("gateway key not deleted")
	}
	must(t, e.c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc))
	if pvc.Annotations[AnnotationDeleteAfter] == "" {
		t.Fatal("PVC not marked for deletion")
	}
	e.now = e.now.Add(8 * 24 * time.Hour)
	e.reconcile("athena")
	err := e.c.Get(ctx, client.ObjectKeyFromObject(pvc), pvc)
	if err == nil && pvc.DeletionTimestamp.IsZero() {
		t.Error("PVC kept past retention")
	}
}

func TestNetworkPolicy(t *testing.T) {
	r := &v1.Room{ObjectMeta: metav1.ObjectMeta{Name: "athena", Namespace: ns}}
	np := desiredNetworkPolicy(r, "acme", Egress{Internet: true, Denied: []string{"10.42.0.0/16", "fd00:42::/56", "bogus"}, Allowed: []string{"192.168.1.10/32"}})
	var inet *networkingv1.NetworkPolicyEgressRule
	for i := range np.Spec.Egress {
		for _, p := range np.Spec.Egress[i].To {
			if p.IPBlock != nil && p.IPBlock.CIDR == "0.0.0.0/0" {
				inet = &np.Spec.Egress[i]
			}
		}
	}
	if inet == nil {
		t.Fatal("no internet rule")
	}
	v4, v6 := inet.To[0].IPBlock, inet.To[1].IPBlock
	if v6.CIDR != "::/0" {
		t.Fatal(v6)
	}
	for _, want := range []string{"10.42.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "192.168.0.0/16"} {
		if !contains(v4.Except, want) {
			t.Errorf("v4 except lacks %s: %v", want, v4.Except)
		}
	}
	if !contains(v6.Except, "fd00:42::/56") || contains(v6.Except, "10.42.0.0/16") {
		t.Errorf("v6 except = %v", v6.Except)
	}
	last := np.Spec.Egress[len(np.Spec.Egress)-1]
	if last.To[0].IPBlock == nil || last.To[0].IPBlock.CIDR != "192.168.1.10/32" {
		t.Errorf("allowed CIDR rule = %+v", last)
	}

	off := desiredNetworkPolicy(r, "acme", Egress{Internet: false})
	for _, rule := range off.Spec.Egress {
		for _, p := range rule.To {
			if p.IPBlock != nil {
				t.Error("internet rule with internet off")
			}
		}
	}
	if len(off.Spec.Egress) != 1+len(RoomPlatformPeers) {
		t.Errorf("egress = %d rules", len(off.Spec.Egress))
	}
}

func contains(s []string, x string) bool {
	for _, y := range s {
		if y == x {
			return true
		}
	}
	return false
}

func TestTiers(t *testing.T) {
	r := &v1.Room{ObjectMeta: metav1.ObjectMeta{Name: "athena", Namespace: ns}}
	s := desiredStatefulSet(r, "acme", "room:1", "sysbox", podExtras{}).Spec.Template.Spec
	if s.RuntimeClassName == nil || *s.RuntimeClassName != "sysbox-runc" || s.HostUsers != nil {
		t.Errorf("sysbox = %+v", s)
	}
	p := desiredStatefulSet(r, "acme", "room:1", "privileged", podExtras{}).Spec.Template.Spec
	if p.Containers[0].SecurityContext == nil || !*p.Containers[0].SecurityContext.Privileged {
		t.Error("privileged tier not privileged")
	}
	u := desiredStatefulSet(r, "acme", "room:1", "userns", podExtras{}).Spec.Template.Spec
	if u.HostUsers == nil || *u.HostUsers || u.Containers[0].SecurityContext.Privileged != nil {
		t.Error("userns tier")
	}
	if TemplateHash(&desiredStatefulSet(r, "acme", "room:1", "userns", podExtras{}).Spec.Template) != TemplateHash(&desiredStatefulSet(r, "acme", "room:1", "userns", podExtras{}).Spec.Template) {
		t.Error("template hash unstable")
	}
}

func TestInWindow(t *testing.T) {
	sat := time.Date(2026, 10, 17, 3, 0, 0, 0, time.UTC) // a Saturday
	cases := []struct {
		w    string
		t    time.Time
		want bool
	}{
		{"Sat 02:00-06:00", sat, true},
		{"Sat 02:00-06:00", sat.Add(4 * time.Hour), false},
		{"Fri 23:00-04:00", sat, true},
		{"Sun 02:00-06:00", sat, false},
		{"garbage", sat, false},
	}
	for _, c := range cases {
		if got := InWindow(c.w, c.t); got != c.want {
			t.Errorf("InWindow(%q, %v) = %v", c.w, c.t, got)
		}
	}
}
