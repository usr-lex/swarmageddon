package runnerpool

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/room/roomtest"
)

const ns = "ws-acme"

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) (client.Client, *Reconciler, *roomtest.Forge) {
	c, op, _ := roomtest.Client(t)
	f := roomtest.NewForge(t)
	r := &Reconciler{Client: op, Deps: deps.Deps{
		Images: &roomtest.Images{RunnerImage: "runner:1"}, Forge: f.Client,
		PlatformTier: func(context.Context) (string, error) { return "userns", nil },
	}}
	ctx := context.Background()
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	must(t, c.Create(ctx, &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1.WorkspaceSpec{Forge: v1.ForgeSpec{Org: "acme"}}}))
	must(t, c.Create(ctx, &v1.RunnerPool{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: ns}, Spec: v1.RunnerPoolSpec{
		Labels: []string{"swarm", "swarm-dirty"}, MaxConcurrent: 2, MinFreeDiskGB: 5,
		Resources: v1.RunnerResources{EphemeralStorage: ptr.To(resource.MustParse("10Gi"))},
	}}))
	return c, r, f
}

func reconcile(t *testing.T, r *Reconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "ci"}}); err != nil {
		t.Fatal(err)
	}
}

func pods(t *testing.T, c client.Client) []corev1.Pod {
	var l corev1.PodList
	must(t, c.List(context.Background(), &l, client.InNamespace(ns), client.MatchingLabels{LabelPool: "ci"}))
	return l.Items
}

func pool(t *testing.T, c client.Client) *v1.RunnerPool {
	var p v1.RunnerPool
	must(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "ci"}, &p))
	return &p
}

func TestStartsOnePodPerJobUpToMax(t *testing.T) {
	c, r, f := setup(t)
	f.SetJobs([]forgejo.RunJob{
		{ID: 1, Status: "waiting", RunsOn: []string{"swarm"}, Handle: "h1"},
		{ID: 2, Status: "waiting", RunsOn: []string{"swarm-dirty"}},
		{ID: 3, Status: "waiting", RunsOn: []string{"swarm"}},
		{ID: 4, Status: "waiting", RunsOn: []string{"ubuntu-latest"}}, // not ours
	})
	reconcile(t, r)
	ps := pods(t, c)
	if len(ps) != 2 {
		t.Fatalf("pods = %d, want 2 (maxConcurrent)", len(ps))
	}
	p := ps[0]
	if p.Name != "runner-ci-1" {
		t.Errorf("first pod = %s", p.Name)
	}
	ctr := p.Spec.Containers[0]
	if ctr.Image != "runner:1" || ctr.Args[0] != "one-job" || !slices.Contains(ctr.Args, "--handle") || !slices.Contains(ctr.Args, "file:/run/runner/token") || !slices.Contains(ctr.Args, "swarm:host") {
		t.Errorf("container = %v %v", ctr.Image, ctr.Args)
	}
	if len(ctr.Command) < 3 || ctr.Command[0] != "/bin/sh" || ctr.Command[len(ctr.Command)-1] != "forgejo-runner" {
		t.Errorf("command = %q, want the wait for Forgejo, then forgejo-runner", ctr.Command)
	}
	if p.Spec.RestartPolicy != corev1.RestartPolicyNever || *p.Spec.AutomountServiceAccountToken || p.Spec.HostUsers == nil || *p.Spec.HostUsers {
		t.Errorf("pod spec = %+v", p.Spec)
	}
	if sl := p.Spec.Volumes[1].EmptyDir.SizeLimit; sl == nil || sl.String() != "10Gi" {
		t.Errorf("emptyDir size limit = %v", sl)
	}
	reg := f.Calls("POST", "/orgs/acme/actions/runners")
	if len(reg) != 2 || reg[0].Body["ephemeral"] != true {
		t.Errorf("registrations = %+v", reg)
	}
	var sec corev1.Secret
	must(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "runner-ci-1"}, &sec))
	if string(sec.Data["token"]) == "" && sec.StringData["token"] == "" {
		t.Error("token secret empty")
	}
	st := pool(t, c).Status
	if st.Running != 2 || st.Waiting != 1 {
		t.Errorf("status = %+v", st)
	}

	// Polling again starts nothing new for jobs already running.
	reconcile(t, r)
	if n := len(f.Calls("POST", "/orgs/acme/actions/runners")); n != 2 {
		t.Errorf("registered %d runners, want 2", n)
	}

	// A finished pod is deleted and frees a slot.
	p.Status.Phase = corev1.PodSucceeded
	must(t, c.Status().Update(context.Background(), &p))
	f.SetJobs([]forgejo.RunJob{{ID: 2, Status: "waiting", RunsOn: []string{"swarm-dirty"}}, {ID: 3, Status: "waiting", RunsOn: []string{"swarm"}}})
	reconcile(t, r)
	names := []string{}
	for _, p := range pods(t, c) {
		if p.DeletionTimestamp.IsZero() {
			names = append(names, p.Name)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"runner-ci-2", "runner-ci-3"}) {
		t.Errorf("pods = %v", names)
	}

	var np networkingv1.NetworkPolicy
	must(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "runners-ci"}, &np))
	if len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 1+len(RunnerPlatformPeers)+1 {
		t.Errorf("policy = %+v", np.Spec)
	}
}

func TestRefusesBelowMinFreeDisk(t *testing.T) {
	c, r, f := setup(t)
	r.FreeDisk = func(context.Context) (int64, bool, error) { return 2 << 30, true, nil }
	f.SetJobs([]forgejo.RunJob{{ID: 1, Status: "waiting", RunsOn: []string{"swarm"}}})
	reconcile(t, r)
	if len(pods(t, c)) != 0 {
		t.Fatal("started a job with low disk")
	}
	p := pool(t, c)
	if !meta.IsStatusConditionTrue(p.Status.Conditions, ConditionDiskLow) || p.Status.Waiting != 1 {
		t.Errorf("status = %+v", p.Status)
	}
	r.FreeDisk = func(context.Context) (int64, bool, error) { return 50 << 30, true, nil }
	reconcile(t, r)
	if len(pods(t, c)) != 1 {
		t.Fatal("did not start once disk freed")
	}
	if meta.IsStatusConditionTrue(pool(t, c).Status.Conditions, ConditionDiskLow) {
		t.Error("DiskLow still true")
	}
}

func TestAllocatableFree(t *testing.T) {
	c, r, _ := setup(t)
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	must(t, c.Create(ctx, n))
	n.Status.Allocatable = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("100Gi")}
	must(t, c.Status().Update(ctx, n))
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "big", Namespace: ns}, Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{
		Name: "c", Image: "x", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("30Gi")}},
	}}}}
	must(t, c.Create(ctx, p))
	free, ok, err := r.allocatableFree(ctx)
	if err != nil || !ok || free != 70<<30 {
		t.Errorf("free = %d %v %v", free>>30, ok, err)
	}
}
