package rbactest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/egress"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/operator/platform"
	"github.com/usr-lex/swarmageddon/internal/operator/preflight"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
	"github.com/usr-lex/swarmageddon/internal/operator/workspace"
	"github.com/usr-lex/swarmageddon/internal/pki"
	"github.com/usr-lex/swarmageddon/internal/warden"
)

const (
	sys  = v1alpha1.SystemNamespace
	plat = v1alpha1.PlatformNamespace
	ws   = "acme"
	wsNS = "ws-acme"
)

// The ServiceAccounts, as the chart and the platform templates name them.
var (
	operatorSA      = sa{sys, "swarmageddon-operator"}
	controlCenterSA = sa{sys, "control-center"}
	welcomeSA       = sa{sys, "welcome"}
	messagingSA     = sa{plat, "messaging"}
	wardenSA        = sa{plat, "warden"}
	egressSA        = sa{plat, "egress"}
)

// The operator's own Secrets in the system namespace; every other Secret
// there is the control center's (v1alpha1.ControlCenterSecret).
var operatorSystemSecrets = []string{pki.CASecretName, "control-center-tls", "control-center-client"}

func TestComponentsCanMakeTheirCalls(t *testing.T) {
	env := testenv.Start(t)
	admin := env.Client
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// What `kubectl apply -f install.yaml` creates, but the Deployments.
	for _, o := range installObjects(t, "Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Service") {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatalf("%s %s: %v", o.GetKind(), o.GetName(), err)
		}
	}
	op, err := client.New(operatorSA.config(env.Config), client.Options{Scheme: env.Scheme, Mapper: env.Mapper})
	if err != nil {
		t.Fatal(err)
	}

	// The operator brings up the platform and a workspace; every Role,
	// RoleBinding and ClusterRole it applies must pass RBAC's escalation
	// check against the chart's operator ClusterRole.
	runPlatform(t, ctx, env, op)
	ca, err := (&pki.Store{Client: op, Namespace: sys}).CA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ws}, Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: ws}}}); err != nil {
		t.Fatal(err)
	}
	wr := &workspace.Reconciler{Client: op, Scheme: env.Scheme, Deps: deps.Deps{PKI: ca, Version: "v0.1.0"}}
	for range 2 {
		if _, err := wr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: ws}}); err != nil {
			t.Fatalf("workspace: %v", err)
		}
	}
	// The workspace controller makes the webhook secret once Forgejo is
	// up, which it isn't here; make it as it does.
	if err := op.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: workspace.WebhookSecretName(ws), Namespace: sys, Labels: map[string]string{
		v1alpha1.LabelManaged: "true", v1alpha1.LabelWorkspace: ws, v1alpha1.LabelComponent: "forge-webhook",
	}}, Data: map[string][]byte{"secret": []byte("hmac")}}); err != nil {
		t.Fatal(err)
	}
	startControlCenterAccess(t, ctx, env)

	t.Run("operator secrets", func(t *testing.T) {
		var l corev1.SecretList
		if err := admin.List(ctx, &l, client.InNamespace(sys)); err != nil {
			t.Fatal(err)
		}
		for _, s := range l.Items {
			if !v1alpha1.ControlCenterSecret(s.Name) && !slices.Contains(operatorSystemSecrets, s.Name) {
				t.Errorf("Secret %s in the system namespace: the control center's (v1alpha1.ControlCenterSecrets) or the operator's (operatorSystemSecrets)?", s.Name)
			}
		}
		for _, n := range operatorSystemSecrets {
			if v1alpha1.ControlCenterSecret(n) {
				t.Errorf("the control center may read the operator's %s", n)
			}
		}
		for _, n := range []string{"control-center-gateway", "forge-reader", "setup-token", "control-center-db", "vapid", "control-center-session"} {
			if err := admin.Get(ctx, types.NamespacedName{Namespace: sys, Name: n}, &corev1.Secret{}); err != nil {
				t.Errorf("Secret %s: %v", n, err)
			}
		}
		var np networkingv1.NetworkPolicy
		if err := admin.Get(ctx, types.NamespacedName{Namespace: sys, Name: "control-center"}, &np); err != nil {
			t.Errorf("control center NetworkPolicy: %v", err)
		}
	})
	t.Run("control center", func(t *testing.T) { controlCenterCalls(t, ctx, env) })
	t.Run("messaging", func(t *testing.T) {
		d, err := directory.NewKube(messagingSA.config(env.Config), slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = d.Start(ctx) }()
		waitFor(t, 30*time.Second, d.Ready, "messaging's directory (workspaces, rooms, roletemplates) never synced")
		if w, ok := d.Workspace(ws); !ok || len(w.Roles) == 0 {
			t.Errorf("directory workspace %+v", w)
		}
		messagingSA.expect(t, admin, false,
			call{verb: "get", resource: "secrets", ns: plat, name: "messaging-tls"},
			call{verb: "list", resource: "secrets", ns: wsNS},
			call{verb: "update", group: "swarmageddon.dev", resource: "rooms", ns: wsNS},
			call{verb: "list", resource: "configmaps", ns: plat},
		)
	})
	t.Run("warden", func(t *testing.T) {
		if err := admin.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "warden-keys", Namespace: plat}}); err != nil {
			t.Fatal(err)
		}
		cfg := wardenSA.config(env.Config)
		synced := warden.WatchTable(ctx, kubernetes.NewForConfigOrDie(cfg), plat, &warden.Proxy{}, slog.New(slog.DiscardHandler))
		waitFor(t, 30*time.Second, synced, "the warden's key table never synced")
		if err := admin.Create(ctx, &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "hermes", Namespace: wsNS},
			Spec: v1alpha1.RoomSpec{Role: "implementer", CLI: v1alpha1.CLIClaude}}); err != nil {
			t.Fatal(err)
		}
		kc, err := client.New(cfg, client.Options{Scheme: env.Scheme, Mapper: env.Mapper})
		if err != nil {
			t.Fatal(err)
		}
		if err := (&warden.RoomQuarantiner{Client: kc}).Quarantine(ctx, ws, "hermes", "test"); err != nil {
			t.Errorf("quarantine: %v", err)
		}
		wardenSA.expect(t, admin, false,
			call{verb: "get", resource: "secrets", ns: plat, name: "warden-client"},
			call{verb: "list", resource: "configmaps", ns: plat},
			call{verb: "get", resource: "configmaps", ns: plat, name: "egress-allowlist"},
			call{verb: "update", group: "swarmageddon.dev", resource: "rooms", ns: wsNS, name: "hermes"},
			call{verb: "patch", group: "swarmageddon.dev", resource: "workspaces", name: ws},
		)
	})
	t.Run("egress", func(t *testing.T) {
		synced := egress.WatchTable(ctx, kubernetes.NewForConfigOrDie(egressSA.config(env.Config)), plat, &egress.Proxy{}, slog.New(slog.DiscardHandler))
		waitFor(t, 30*time.Second, synced, "the egress allowlist never synced")
		egressSA.expect(t, admin, false,
			call{verb: "get", resource: "configmaps", ns: plat, name: "warden-keys"},
			call{verb: "list", resource: "secrets", ns: plat},
		)
	})
	t.Run("welcome", func(t *testing.T) {
		// cmd/operator/welcome.go reads these two objects only.
		welcomeSA.expect(t, admin, true,
			call{verb: "get", group: "swarmageddon.dev", resource: "platforms", name: v1alpha1.PlatformName},
			call{verb: "get", resource: "secrets", ns: sys, name: "setup-token"},
		)
		welcomeSA.expect(t, admin, false,
			call{verb: "get", resource: "secrets", ns: sys, name: "vapid"},
			call{verb: "get", group: "swarmageddon.dev", resource: "workspaces", name: ws},
		)
	})
}

// controlCenterCalls makes the calls internal/controlcenter makes, through
// its own client where there is one.
func controlCenterCalls(t *testing.T, ctx context.Context, env *testenv.Env) {
	admin := env.Client
	kc, err := k8s.New(controlCenterSA.config(env.Config))
	if err != nil {
		t.Fatal(err)
	}
	ok := func(what string, err error) {
		t.Helper()
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("%s: %v", what, err)
		}
	}
	forbidden := func(what string, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s: want Forbidden, got %v", what, err)
		}
	}

	// Cluster-wide: the Platform and Workspaces.
	_, err = kc.Platform(ctx)
	ok("get platform", err)
	_, err = kc.UpdatePlatform(ctx, func(p *v1alpha1.Platform) error {
		p.Annotations = map[string]string{platform.AnnotationRerunPreflight: "now"}
		return nil
	})
	ok("update platform", err)
	ok("claim admin", kc.SetAdminClaimed(ctx))
	_, err = kc.Workspaces(ctx)
	ok("list workspaces", err)
	ok("create workspace", kc.C.Create(ctx, &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "beta"},
		Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: "beta"}}}))
	w, err := kc.Workspace(ctx, ws)
	ok("get workspace", err)
	if w != nil {
		w.Spec.Models.Budgets.PerRoomMonthlyUSD = 50
		ok("update workspace", kc.C.Update(ctx, w))
	}

	// In the workspace namespace, through the operator's RoleBinding.
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "athena", Namespace: wsNS},
		Spec: v1alpha1.RoomSpec{Role: "implementer", CLI: v1alpha1.CLIClaude}}
	ok("create room", kc.C.Create(ctx, room))
	_, err = kc.Rooms(ctx, ws)
	ok("list rooms", err)
	if r, err := kc.Room(ctx, ws, "athena"); err == nil {
		r.Annotations = map[string]string{v1alpha1.AnnotationRecreate: "now"}
		ok("update room", kc.C.Update(ctx, r))
	} else {
		ok("get room", err)
	}
	_, err = kc.RoleTemplates(ctx, ws)
	ok("list role templates", err)
	if rt, err := kc.RoleTemplate(ctx, ws, "implementer"); err == nil {
		orig := rt.DeepCopy()
		rt.Spec.RoomDefaults.Env = map[string]string{"FOO": "bar"}
		ok("patch role template", kc.C.Patch(ctx, rt, client.MergeFrom(orig)))
	} else {
		t.Errorf("get role template: %v", err)
	}
	ok("create runner pool", kc.C.Create(ctx, &v1alpha1.RunnerPool{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: wsNS},
		Spec: v1alpha1.RunnerPoolSpec{Labels: []string{"swarm"}}}))
	_, err = kc.RoomPods(ctx, ws)
	ok("list pods", err)
	_, err = kc.RoomPod(ctx, ws, "athena")
	ok("get pod", err)
	_, err = kc.RoomClaims(ctx, ws, "athena")
	ok("get claims", err)
	_, err = kc.Kube.CoreV1().Pods(wsNS).GetLogs(k8s.PodName("athena"), &corev1.PodLogOptions{}).Stream(ctx)
	ok("pod logs", err)
	ok("list events", kc.C.List(ctx, &corev1.EventList{}, client.InNamespace(wsNS)))
	_, err = kc.RoomSecretKeys(ctx, ws)
	ok("room secret keys", err)
	ok("set room secret", kc.SetRoomSecretKey(ctx, ws, "TOKEN", []byte("x")))
	ok("delete room", kc.C.Delete(ctx, room))
	// Terminals, VNC and metrics: no kubelet or metrics server in envtest.
	controlCenterSA.expect(t, admin, true,
		call{verb: "create", resource: "pods", sub: "exec", ns: wsNS, name: k8s.PodName("athena")},
		call{verb: "create", resource: "pods", sub: "portforward", ns: wsNS, name: k8s.PodName("athena")},
		call{verb: "get", group: "metrics.k8s.io", resource: "pods", ns: wsNS, name: k8s.PodName("athena")},
		call{verb: "watch", resource: "events", ns: wsNS},
	)

	// Its Secrets and ConfigMaps in the system namespace.
	for _, name := range v1alpha1.ControlCenterSecrets {
		_, err := kc.GetSecret(ctx, name)
		ok("get secret "+name, err)
	}
	for range 2 {
		ok("apply local-admin", kc.ApplySecret(ctx, k8s.SecretLocalAdmin, map[string][]byte{"username": []byte("admin")}))
		_, err := kc.CreateSecretIfMissing(ctx, k8s.SecretSAML, map[string][]byte{"key": []byte("k")})
		ok("create saml key", err)
	}
	// Per-provider names: the second apply updates, once the operator has
	// granted the new name.
	for _, name := range []string{"gateway-provider-anthropic", "oidc-google", "notify-acme-slack"} {
		ok("create "+name, kc.ApplySecret(ctx, name, map[string][]byte{"v": []byte("1")}))
		ok("update "+name, kc.ApplySecret(ctx, name, map[string][]byte{"v": []byte("2")}))
		if d, err := kc.GetSecret(ctx, name); err != nil || string(d["v"]) != "2" {
			t.Errorf("get %s: %v %v", name, d, err)
		}
	}
	waitFor(t, 10*time.Second, func() bool {
		_, err := kc.GetSecret(ctx, workspace.WebhookSecretName(ws))
		return err == nil
	}, "the control center can't read "+workspace.WebhookSecretName(ws))
	for range 2 {
		ok("apply saml-okta", kc.ApplyConfigMap(ctx, "saml-okta", map[string]string{"metadata.xml": "<x/>"}))
	}
	_, err = kc.GetConfigMap(ctx, "saml-okta")
	ok("get saml-okta", err)
	_, err = kc.GetConfigMap(ctx, "platform-ca-bundle")
	ok("get platform-ca-bundle", err)

	// Refused: a Secret it creates under a name that isn't its own stays
	// unreadable (the operator has granted the names applied after it).
	squat := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "platform-ca-2", Namespace: sys,
		Labels: map[string]string{v1alpha1.LabelComponent: "control-center"}}}
	ok("create squatter", kc.C.Create(ctx, squat))
	ok("create oidc-later", kc.ApplySecret(ctx, "oidc-later", nil))
	ok("update oidc-later", kc.ApplySecret(ctx, "oidc-later", map[string][]byte{"v": []byte("1")}))
	forbidden("get squatter", kc.C.Get(ctx, client.ObjectKeyFromObject(squat), &corev1.Secret{}))
	forbidden("get platform-ca", kc.C.Get(ctx, types.NamespacedName{Namespace: sys, Name: pki.CASecretName}, &corev1.Secret{}))
	if err := kc.C.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: pki.CASecretName, Namespace: sys}}); !apierrors.IsAlreadyExists(err) {
		t.Errorf("create platform-ca: %v", err)
	}
	controlCenterSA.expect(t, admin, false,
		call{verb: "list", resource: "secrets", ns: sys},
		call{verb: "watch", resource: "secrets", ns: sys},
		call{verb: "update", resource: "secrets", ns: sys, name: pki.CASecretName},
		call{verb: "get", resource: "secrets", ns: sys, name: "control-center-tls"},
		call{verb: "get", resource: "secrets", ns: plat, name: "forgejo-admin"},
		call{verb: "get", resource: "secrets", ns: plat, name: "gateway-master"},
		call{verb: "get", resource: "secrets", ns: wsNS, name: "room-athena-identity"},
		call{verb: "list", resource: "secrets", ns: wsNS},
		call{verb: "create", resource: "pods", ns: wsNS},
		call{verb: "list", resource: "pods"},
		call{verb: "delete", resource: "namespaces", name: wsNS},
		call{verb: "update", group: "rbac.authorization.k8s.io", resource: "roles", ns: sys, name: v1alpha1.ControlCenterOwnedRole},
		call{verb: "get", resource: "configmaps", ns: plat, name: "warden-keys"},
		call{verb: "list", resource: "configmaps", ns: sys},
		call{verb: "update", group: "swarmageddon.dev", resource: "workspaces", sub: "status", name: ws},
	)
}

// runPlatform reconciles the Platform as the operator, playing the
// kubelet, until every platform service is ready.
func runPlatform(t *testing.T, ctx context.Context, env *testenv.Env, op client.Client) {
	t.Helper()
	admin := env.Client
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{corev1.LabelArchStable: "amd64"}}}
	must(t, admin.Create(ctx, node))
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.5.10"}}
	must(t, admin.Status().Update(ctx, node))
	must(t, admin.Create(ctx, &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "traefik", Annotations: map[string]string{networkingv1.AnnotationIsDefaultIngressClass: "true"}},
		Spec:       networkingv1.IngressClassSpec{Controller: "traefik.io/ingress-controller"},
	}))
	// k3s's Traefik, so the operator also writes a ServersTransport and the
	// default TLSStore.
	_, err := envtest.InstallCRDs(env.Config, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{
		traefikCRD("serverstransports", "ServersTransport"), traefikCRD("tlsstores", "TLSStore"),
	}})
	must(t, err)

	imgs := images.Defaults("v0.1.0")
	dc := discovery.NewDiscoveryClientForConfigOrDie(env.Config)
	litellm := fakeLiteLLM(t)
	r := &platform.Reconciler{
		Client: op, Scheme: env.Scheme, Out: io.Discard, ForgejoURL: fakeForgejo(t),
		Deps: deps.Deps{CAStore: &pki.Store{Client: op, Namespace: sys}, Pinned: imgs, Version: "v0.1.0",
			Gateway: func(context.Context) (*gateway.Client, error) { return gateway.New(litellm, "sk-master"), nil }},
		Preflight: &preflight.Runner{Client: op, Images: imgs, ServerVersion: dc.ServerVersion},
	}
	must(t, platform.EnsurePlatform(ctx, op))
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.PlatformName}}); err != nil {
			t.Fatalf("platform: %v", err)
		}
	}
	reconcile()
	var p v1alpha1.Platform
	must(t, admin.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p))
	p.Spec.Wizard.ServicesApproved = true
	must(t, admin.Update(ctx, &p))
	for range 20 {
		reconcile()
		must(t, admin.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p))
		if meta.IsStatusConditionTrue(p.Status.Conditions, v1alpha1.ConditionReady) {
			for kind, name := range map[string]string{"ServersTransport": "control-center", "TLSStore": "default"} {
				var o unstructured.Unstructured
				o.SetAPIVersion("traefik.io/v1alpha1")
				o.SetKind(kind)
				if err := admin.Get(ctx, types.NamespacedName{Namespace: sys, Name: name}, &o); err != nil {
					t.Errorf("%s %s: %v", kind, name, err)
				}
			}
			return
		}
		playKubelet(t, ctx, admin)
	}
	t.Fatalf("platform never ready: %+v", p.Status.Conditions)
}

// traefikCRD is a schemaless traefik.io/v1alpha1 CRD.
func traefikCRD(plural, kind string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".traefik.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "traefik.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Singular: strings.ToLower(kind), Kind: kind, ListKind: kind + "List"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr.To(true)}}}},
		},
	}
}

// playKubelet reports every platform workload ready and every Job done.
func playKubelet(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	var sts appsv1.StatefulSetList
	must(t, c.List(ctx, &sts, client.InNamespace(plat)))
	for i := range sts.Items {
		s := &sts.Items[i]
		n := ptr.Deref(s.Spec.Replicas, 1)
		s.Status.Replicas, s.Status.ReadyReplicas, s.Status.AvailableReplicas, s.Status.CurrentReplicas, s.Status.UpdatedReplicas = n, n, n, n, n
		s.Status.ObservedGeneration = s.Generation
		must(t, c.Status().Update(ctx, s))
	}
	var ds appsv1.DeploymentList
	must(t, c.List(ctx, &ds, client.InNamespace(plat)))
	for i := range ds.Items {
		d := &ds.Items[i]
		n := ptr.Deref(d.Spec.Replicas, 1)
		d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas = n, n, n, n
		d.Status.ObservedGeneration = d.Generation
		must(t, c.Status().Update(ctx, d))
	}
	var jobs batchv1.JobList
	must(t, c.List(ctx, &jobs, client.InNamespace(plat)))
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Status.Succeeded > 0 {
			continue
		}
		now := metav1.Now()
		j.Status.StartTime, j.Status.CompletionTime, j.Status.Succeeded = &now, &now, 1
		j.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		must(t, c.Status().Update(ctx, j))
	}
}

// startControlCenterAccess runs the operator's control-center-owned
// controller in a manager, as the operator, until the test ends.
func startControlCenterAccess(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	mgr, err := ctrl.NewManager(operatorSA.config(env.Config), ctrl.Options{
		Scheme: env.Scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	must(t, err)
	must(t, platform.SetupControlCenterAccess(mgr, deps.Deps{Version: "v0.1.0"}))
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			t.Errorf("manager: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// fakeForgejo issues tokens like POST /api/v1/users/{u}/tokens.
func fakeForgejo(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/tokens") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"sha1": "tok"})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// fakeLiteLLM issues keys and view-only users.
func fakeLiteLLM(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/generate", "/user/new":
			_ = json.NewEncoder(w).Encode(map[string]string{"key": "sk-" + strings.ReplaceAll(r.URL.Path, "/", "")})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
