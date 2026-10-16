package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// fakeForge is an in-memory Forgejo with just enough state for the
// workspace controller: orgs, teams, repos, and list endpoints.
type fakeForge struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any
	orgs   map[string]bool
	teams  []forgejo.Team
	repos  map[string]bool
	hooks  []forgejo.Hook
}

func newFakeForge(t *testing.T) (*fakeForge, *httptest.Server) {
	f := &fakeForge{orgs: map[string]bool{}, repos: map[string]bool{}, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		call := r.Method + " " + path
		f.calls = append(f.calls, call)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.bodies[call] = body
		w.Header().Set("Content-Type", "application/json")
		write := func(code int, v any) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(v) }
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case r.Method == "GET" && len(parts) == 2 && parts[0] == "orgs":
			if !f.orgs[parts[1]] {
				write(404, map[string]string{"message": "not found"})
				return
			}
			write(200, forgejo.Organization{Name: parts[1]})
		case r.Method == "POST" && path == "/orgs":
			f.orgs[body["username"].(string)] = true
			write(201, forgejo.Organization{Name: body["username"].(string)})
		case r.Method == "GET" && len(parts) == 3 && parts[2] == "teams":
			write(200, f.teams)
		case r.Method == "POST" && len(parts) == 3 && parts[2] == "teams":
			team := forgejo.Team{ID: int64(len(f.teams) + 1), Name: body["name"].(string), Permission: body["permission"].(string),
				IncludesAllRepositories: body["includes_all_repositories"].(bool), Description: body["description"].(string)}
			f.teams = append(f.teams, team)
			write(201, team)
		case r.Method == "GET" && parts[0] == "users":
			write(404, map[string]string{"message": "not found"})
		case r.Method == "GET" && len(parts) == 3 && parts[0] == "repos":
			if !f.repos[parts[2]] {
				write(404, map[string]string{"message": "not found"})
				return
			}
			write(200, forgejo.Repository{Name: parts[2], DefaultBranch: "main"})
		case r.Method == "POST" && len(parts) == 3 && parts[2] == "repos":
			f.repos[body["name"].(string)] = true
			write(201, forgejo.Repository{Name: body["name"].(string), DefaultBranch: "main"})
		case r.Method == "GET" && strings.HasSuffix(path, "/branch_protections/main"):
			write(404, map[string]string{"message": "not found"})
		case r.Method == "GET" && strings.HasSuffix(path, "/hooks"):
			write(200, f.hooks)
		case r.Method == "POST" && strings.HasSuffix(path, "/hooks"):
			cfg := map[string]string{}
			for k, v := range body["config"].(map[string]any) {
				cfg[k] = fmt.Sprint(v)
			}
			var evs []string
			for _, e := range body["events"].([]any) {
				evs = append(evs, e.(string))
			}
			f.hooks = append(f.hooks, forgejo.Hook{ID: 1, Type: "forgejo", Active: true, Events: evs, Config: map[string]string{"url": cfg["url"], "content_type": cfg["content_type"]}})
			write(201, f.hooks[len(f.hooks)-1])
		case r.Method == "GET":
			write(200, []any{})
		case r.Method == "PUT":
			w.WriteHeader(204)
		default:
			write(201, map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeForge) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestWorkspaceReconcile(t *testing.T) {
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
	var gwMu sync.Mutex
	var gwBodies []map[string]any
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/team/info" {
			http.Error(w, `{"error":"team not found"}`, http.StatusNotFound)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		gwMu.Lock()
		gwBodies = append(gwBodies, b)
		gwMu.Unlock()
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(gwSrv.Close)

	ca, err := pki.NewCA("test", metav1.Now().Time)
	must(err)
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme, Deps: deps.Deps{
		PKI:     ca,
		Version: "v0.1.0",
		Forge:   func(context.Context) (*forgejo.Client, error) { return forgejo.New(forgeSrv.URL, "admin"), nil },
		Gateway: func(context.Context) (*gateway.Client, error) { return gateway.New(gwSrv.URL, "sk-master"), nil },
	}}

	w := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: v1alpha1.WorkspaceSpec{
			DisplayName:  "Acme",
			Forge:        v1alpha1.ForgeSpec{Org: "acme", Teams: []v1alpha1.TeamSpec{{Name: "leads", Members: []string{"hermes"}}}},
			Repositories: []v1alpha1.RepositorySpec{{Name: "api", StatusChecks: []string{"ci/test"}}},
			Models:       v1alpha1.ModelSettings{Budgets: v1alpha1.BudgetSettings{PerRoomMonthlyUSD: 200}},
		},
	}
	must(c.Create(ctx, w))
	reconcileOnce := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "acme"}}); err != nil {
			t.Fatal(err)
		}
	}
	reconcileOnce()

	ns := "ws-acme"
	var nsObj corev1.Namespace
	must(c.Get(ctx, types.NamespacedName{Name: ns}, &nsObj))
	if nsObj.Labels[v1alpha1.LabelWorkspace] != "acme" || nsObj.OwnerReferences[0].Name != "acme" {
		t.Fatalf("namespace metadata %+v", nsObj.ObjectMeta)
	}
	var nps networkingv1.NetworkPolicyList
	must(c.List(ctx, &nps, client.InNamespace(ns)))
	if len(nps.Items) != 2 {
		t.Fatalf("%d network policies", len(nps.Items))
	}
	var role rbacv1.Role
	must(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "control-center"}, &role))
	var rb rbacv1.RoleBinding
	must(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "control-center"}, &rb))
	if rb.Subjects[0].Namespace != v1alpha1.SystemNamespace {
		t.Fatalf("binding subject %+v", rb.Subjects)
	}
	var bundle corev1.ConfigMap
	must(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "platform-ca-bundle"}, &bundle))
	var quota corev1.ResourceQuota
	must(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "workspace"}, &quota))

	var rts v1alpha1.RoleTemplateList
	must(c.List(ctx, &rts, client.InNamespace(ns)))
	if len(rts.Items) != 6 {
		t.Fatalf("%d role templates", len(rts.Items))
	}
	// Templates people edited are not overwritten.
	var rev v1alpha1.RoleTemplate
	must(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "reviewer"}, &rev))
	if !strings.Contains(rev.Spec.RoleText, "forge@acme") || !strings.Contains(rev.Spec.RoleText, "tea pulls approve") {
		t.Fatal("role text lacks the working agreement")
	}
	rev.Spec.RoleText = "custom"
	must(c.Update(ctx, &rev))

	var sec corev1.Secret
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: WebhookSecretName("acme")}, &sec))
	if len(sec.Data["secret"]) != 64 {
		t.Fatal("webhook secret")
	}

	if forge.count("POST /orgs") != 1 || forge.count("POST /orgs/acme/repos") != 1 || forge.count("POST /repos/acme/api/branch_protections") != 1 {
		t.Fatalf("forge calls: %v", forge.calls)
	}
	if forge.count("POST /repos/acme/api/tag_protections") != 1 {
		t.Fatalf("no tag protection: %v", forge.calls)
	}
	bp := forge.bodies["POST /repos/acme/api/branch_protections"]
	if bp["required_approvals"].(float64) != 1 || bp["require_signed_commits"] != true || bp["enable_push"] != false {
		t.Fatalf("protection body %v", bp)
	}
	hook := forge.bodies["POST /orgs/acme/hooks"]
	if hook["config"].(map[string]any)["url"] != WebhookURL("acme") || hook["config"].(map[string]any)["secret"] != string(sec.Data["secret"]) {
		t.Fatalf("hook body %v", hook)
	}
	teamNames := map[string]bool{}
	for _, tm := range forge.teams {
		teamNames[tm.Name] = true
	}
	for _, n := range []string{"implementers", "reviewers", "mergers", "ci", "observers", "leads"} {
		if !teamNames[n] {
			t.Errorf("team %s missing", n)
		}
	}
	gwMu.Lock()
	if len(gwBodies) == 0 || gwBodies[0]["team_id"] != "ws-acme" {
		t.Fatalf("gateway team: %v", gwBodies)
	}
	gwMu.Unlock()

	var got v1alpha1.Workspace
	must(c.Get(ctx, types.NamespacedName{Name: "acme"}, &got))
	for _, typ := range []string{v1alpha1.ConditionNamespaceReady, v1alpha1.ConditionForgeOrgReady, ConditionRepositoriesReady, ConditionForgeWebhookReady, ConditionGatewayTeamReady, ConditionRoleTemplatesReady} {
		if !meta.IsStatusConditionTrue(got.Status.Conditions, typ) {
			t.Errorf("condition %s not true: %+v", typ, meta.FindStatusCondition(got.Status.Conditions, typ))
		}
	}
	if got.Status.Namespace != ns {
		t.Fatal("status.namespace")
	}

	// Second pass: idempotent, no duplicate org, hook or repo; edits kept.
	reconcileOnce()
	if forge.count("POST /orgs") != 1 || forge.count("POST /orgs/acme/hooks") != 1 || forge.count("POST /orgs/acme/repos") != 1 {
		t.Fatalf("not idempotent: %v", forge.calls)
	}
	must(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "reviewer"}, &rev))
	if rev.Spec.RoleText != "custom" {
		t.Fatal("edited role template was overwritten")
	}
}

func TestForgeNotReadyKeepsNamespace(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	_ = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SystemNamespace}})
	ca, _ := pki.NewCA("test", metav1.Now().Time)
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme, Deps: deps.Deps{
		PKI: ca,
		Forge: func(context.Context) (*forgejo.Client, error) {
			return nil, fmt.Errorf("forgejo: %w", deps.ErrNotReady)
		},
	}}
	w := &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "beta"}, Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: "beta"}}}
	if err := c.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "beta"}})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
	var got v1alpha1.Workspace
	_ = c.Get(ctx, types.NamespacedName{Name: "beta"}, &got)
	if !meta.IsStatusConditionTrue(got.Status.Conditions, v1alpha1.ConditionNamespaceReady) {
		t.Fatal("namespace should be ready without the forge")
	}
	if cnd := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionForgeOrgReady); cnd == nil || cnd.Reason != "ForgeNotReady" {
		t.Fatalf("forge condition %+v", cnd)
	}
}
