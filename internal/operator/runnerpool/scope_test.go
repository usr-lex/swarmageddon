package runnerpool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/room/roomtest"
)

func TestGroupPoolServesOnlyItsTeamsRepos(t *testing.T) {
	c, op, _ := roomtest.Client(t)
	f := roomtest.NewForge(t, "implementers", "group-dirty")
	r := &Reconciler{Client: op, FreeDisk: func(context.Context) (int64, bool, error) { return 100 << 30, true, nil }, Deps: deps.Deps{
		Images: &roomtest.Images{RunnerImage: "runner:1"}, Forge: f.Client,
		PlatformTier: func(context.Context) (string, error) { return "userns", nil },
	}}
	ctx := context.Background()
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	must(t, c.Create(ctx, &v1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1.WorkspaceSpec{Forge: v1.ForgeSpec{Org: "acme"}}}))
	must(t, c.Create(ctx, &v1.RunnerPool{ObjectMeta: metav1.ObjectMeta{Name: "dirty", Namespace: ns}, Spec: v1.RunnerPoolSpec{
		Labels: []string{"swarm"}, Group: "dirty", MaxConcurrent: 5,
	}}))
	dirty := forgejo.Repository{ID: 10, Name: "scraper", FullName: "acme/scraper"}
	dirty2 := forgejo.Repository{ID: 12, Name: "lab", FullName: "acme/lab"}
	clean := forgejo.Repository{ID: 11, Name: "api", FullName: "acme/api"}
	f.SetRepos([]forgejo.Repository{dirty, dirty2, clean}, map[int64][]forgejo.Repository{2: {dirty, dirty2}})
	f.SetJobs([]forgejo.RunJob{
		{ID: 1, RepoID: 10, Status: "waiting", RunsOn: []string{"swarm"}},
		{ID: 2, RepoID: 11, Status: "waiting", RunsOn: []string{"swarm"}}, // another team's repo
		{ID: 3, RepoID: 10, Status: "waiting", RunsOn: []string{"group-dirty"}},
	})
	rec := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "dirty"}}); err != nil {
			t.Fatal(err)
		}
	}
	podNames := func() []string {
		var l corev1.PodList
		must(t, c.List(ctx, &l, client.InNamespace(ns), client.MatchingLabels{LabelPool: "dirty"}))
		var names []string
		for _, p := range l.Items {
			names = append(names, p.Name)
		}
		slices.Sort(names)
		return names
	}
	rec()
	if got := podNames(); !slices.Equal(got, []string{"runner-dirty-1", "runner-dirty-3"}) {
		t.Fatalf("pods = %v", got)
	}
	var p corev1.Pod
	must(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "runner-dirty-1"}, &p))
	if p.Annotations[AnnRunnerRepo] != "acme/scraper" || !slices.Contains(p.Spec.Containers[0].Args, "group-dirty:host") {
		t.Errorf("pod: annotations %v args %v", p.Annotations, p.Spec.Containers[0].Args)
	}
	if n := len(f.Calls("POST", "/repos/acme/scraper/actions/runners")); n != 2 {
		t.Errorf("repository registrations = %d, want 2", n)
	}
	if n := len(f.Calls("POST", "/orgs/acme/actions/runners")); n != 0 {
		t.Errorf("org registrations = %d, want 0", n)
	}
	if q := f.Calls("GET", "/orgs/acme/actions/runners/jobs"); len(q) == 0 || !strings.Contains(q[0].Path, "group-dirty") {
		t.Errorf("job query = %+v", q)
	}

	// spec.repos narrows the group's repositories; a repository outside the
	// group stays out even if listed.
	var pool v1.RunnerPool
	must(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "dirty"}, &pool))
	pool.Spec.Repos = []string{"lab", "api"}
	must(t, c.Update(ctx, &pool))
	f.SetJobs([]forgejo.RunJob{
		{ID: 4, RepoID: 10, Status: "waiting", RunsOn: []string{"swarm"}},
		{ID: 5, RepoID: 11, Status: "waiting", RunsOn: []string{"swarm"}},
		{ID: 6, RepoID: 12, Status: "waiting", RunsOn: []string{"swarm"}},
	})
	rec()
	if got := podNames(); !slices.Equal(got, []string{"runner-dirty-1", "runner-dirty-3", "runner-dirty-6"}) {
		t.Fatalf("pods after narrowing = %v", got)
	}

	// A finished group runner is unregistered on its repository.
	p.Status.Phase = corev1.PodSucceeded
	must(t, c.Status().Update(ctx, &p))
	rec()
	if n := len(f.Calls("DELETE", "/repos/acme/scraper/actions/runners/")); n != 1 {
		t.Errorf("repository runner deletions = %d, want 1", n)
	}
}

func TestNodeFreeFromKubeletSummary(t *testing.T) {
	c, r, _ := setup(t)
	ctx := context.Background()
	for _, name := range []string{"n1", "n2"} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		must(t, c.Create(ctx, n))
		n.Status.Allocatable = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("100Gi")}
		must(t, c.Status().Update(ctx, n))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/v1/nodes/n1/proxy/stats/summary" {
			_, _ = w.Write([]byte(`{"node":{"nodeName":"n1","fs":{"availableBytes":` + strconv.FormatInt(12<<30, 10) + `,"capacityBytes":1}}}`))
			return
		}
		http.Error(w, "kubelet unreachable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	sum, err := KubeletSummary(&rest.Config{Host: srv.URL})
	must(t, err)
	r.Summary = sum
	free, ok, err := r.nodeFree(ctx)
	if err != nil || !ok || free != 12<<30 {
		t.Errorf("free = %d GB %v %v", free>>30, ok, err)
	}

	// No node answers: the allocatable fallback.
	r.Summary = func(context.Context, string) ([]byte, error) { return nil, errors.New("forbidden") }
	free, ok, err = r.nodeFree(ctx)
	if err != nil || !ok || free != 100<<30 {
		t.Errorf("fallback free = %d GB %v %v", free>>30, ok, err)
	}
}

// A node running the workspace's pods with under 15% of its disk free raises
// the workspace-wide diskLow (design §12), whether or not jobs wait.
func TestNodeDiskLow(t *testing.T) {
	c, r, _ := setup(t)
	ctx := context.Background()
	for _, name := range []string{"n1", "n2"} {
		must(t, c.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}
	must(t, c.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "room-athena-0", Namespace: ns},
		Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "c", Image: "x"}}}}))
	free := map[string]int64{"n1": 10, "n2": 1} // percent; n2 runs nothing of ours
	r.Summary = func(_ context.Context, node string) ([]byte, error) {
		return []byte(`{"node":{"fs":{"availableBytes":` + strconv.FormatInt(free[node]<<30, 10) + `,"capacityBytes":` + strconv.FormatInt(100<<30, 10) + `}}}`), nil
	}
	reconcile(t, r)
	cond := meta.FindStatusCondition(pool(t, c).Status.Conditions, ConditionNodeDiskLow)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "n1") || strings.Contains(cond.Message, "n2") {
		t.Fatalf("condition %+v", cond)
	}
	free["n1"] = 40
	reconcile(t, r)
	if meta.IsStatusConditionTrue(pool(t, c).Status.Conditions, ConditionNodeDiskLow) {
		t.Error("still low at 40% free")
	}
}
