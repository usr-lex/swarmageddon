package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/operator/preflight"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// fakeForgejo answers POST /api/v1/users/{u}/tokens with basic auth.
func fakeForgejo(t *testing.T, user, pass string) (*httptest.Server, *[]string) {
	var scopes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/users/"+user+"/tokens" || !ok || u != user || p != pass {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		var body struct {
			Name   string   `json:"name"`
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		scopes = append(scopes, strings.Join(body.Scopes, ","))
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sha1": "tok-" + body.Name, "scopes": body.Scopes})
	}))
	t.Cleanup(srv.Close)
	return srv, &scopes
}

func TestPlatformReconcile(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	sys := v1alpha1.SystemNamespace

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sys}}))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{corev1.LabelArchStable: "amd64"}}}
	must(c.Create(ctx, node))
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.5.10"}}
	must(c.Status().Update(ctx, node))
	must(c.Create(ctx, &networkingv1.IngressClass{
		ObjectMeta: metav1.ObjectMeta{Name: "traefik", Annotations: map[string]string{networkingv1.AnnotationIsDefaultIngressClass: "true"}},
		Spec:       networkingv1.IngressClassSpec{Controller: "traefik.io/ingress-controller"},
	}))
	must(c.Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: sys, Name: "control-center"},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: 443}}, Selector: map[string]string{"app": "cc"}},
	}))

	imgs := images.Defaults("v0.1.0")
	dc := discovery.NewDiscoveryClientForConfigOrDie(env.Config)
	var out bytes.Buffer
	// The reconciler runs with exactly the generated operator ClusterRole.
	oc := env.OperatorClient(t)
	r := &Reconciler{
		Client: oc, Scheme: env.Scheme, Out: &out,
		Deps:      deps.Deps{CAStore: &pki.Store{Client: oc, Namespace: sys}, Pinned: imgs, Version: "v0.1.0"},
		Preflight: &preflight.Runner{Client: oc, Images: imgs, ServerVersion: dc.ServerVersion},
	}
	must(EnsurePlatform(ctx, c))
	reconcileOnce := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.PlatformName}}); err != nil {
			t.Fatal(err)
		}
	}
	getPlatform := func() *v1alpha1.Platform {
		t.Helper()
		var p v1alpha1.Platform
		must(c.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p))
		return &p
	}
	secret := func(ns, name string) *corev1.Secret {
		t.Helper()
		var s corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			t.Fatal(err)
		}
		return &s
	}

	reconcileOnce()
	p := getPlatform()
	ca := secret(sys, pki.CASecretName)
	if ca == nil {
		t.Fatal("no CA secret")
	}
	fp, _ := pki.Fingerprint(ca.Data[pki.CACertKey])
	if p.Status.CAFingerprint != fp || p.Status.URL != "https://192.168.5.10/" {
		t.Fatalf("status: fingerprint %q url %q", p.Status.CAFingerprint, p.Status.URL)
	}
	for _, name := range []string{"control-center-tls", "control-center-client", "control-center-session", "vapid", SetupTokenSecret} {
		if secret(sys, name) == nil {
			t.Errorf("secret %s missing", name)
		}
	}
	tls := secret(sys, "control-center-tls")
	cert, err := pki.ParseCert(tls.Data[pki.TLSCertKey])
	must(err)
	if len(cert.IPAddresses) != 2 || cert.IPAddresses[1].String() != "192.168.5.10" {
		t.Errorf("serving cert IPs %v", cert.IPAddresses)
	}
	token := string(secret(sys, SetupTokenSecret).Data["token"])
	if !strings.Contains(out.String(), "WELCOME") || !strings.Contains(out.String(), token) || !strings.Contains(out.String(), fp) {
		t.Fatalf("welcome block:\n%s", out.String())
	}
	var ing networkingv1.Ingress
	must(c.Get(ctx, types.NamespacedName{Namespace: sys, Name: "control-center"}, &ing))
	if ing.OwnerReferences[0].Name != v1alpha1.PlatformName || ing.Labels[v1alpha1.LabelVersion] != "v0.1.0" {
		t.Errorf("ingress metadata %+v", ing.ObjectMeta)
	}
	if p.Status.Preflight == nil || len(p.Status.Preflight.Checks) == 0 {
		t.Fatal("no preflight status")
	}
	var pods corev1.PodList
	must(c.List(ctx, &pods, client.InNamespace(preflight.Namespace)))
	if len(pods.Items) == 0 {
		t.Fatal("no probe pods")
	}
	var bundle corev1.ConfigMap
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "platform-ca-bundle"}, &bundle))

	// Idempotent, and the token is printed only once.
	out.Reset()
	reconcileOnce()
	if out.Len() != 0 || string(secret(sys, SetupTokenSecret).Data["token"]) != token {
		t.Fatal("second reconcile re-issued the token")
	}

	// The control center claims admin: the token goes away.
	p = getPlatform()
	p.Status.AdminClaimed = true
	must(c.Status().Update(ctx, p))
	reconcileOnce()
	if secret(sys, SetupTokenSecret) != nil {
		t.Fatal("token kept after admin was claimed")
	}
	if !getPlatform().Status.AdminClaimed {
		t.Fatal("operator cleared adminClaimed")
	}

	// Reset: a new token, adminClaimed false, annotation removed.
	p = getPlatform()
	p.Annotations = map[string]string{v1alpha1.AnnotationResetSetupToken: "now"}
	must(c.Update(ctx, p))
	reconcileOnce()
	p = getPlatform()
	if p.Status.AdminClaimed || p.Annotations[v1alpha1.AnnotationResetSetupToken] != "" {
		t.Fatalf("after reset: claimed=%v annotations=%v", p.Status.AdminClaimed, p.Annotations)
	}
	if s := secret(sys, SetupTokenSecret); s == nil || string(s.Data["token"]) == token {
		t.Fatal("no new token after reset")
	}

	// Services: approve, then play the kubelet for each step.
	p.Spec.Wizard.ServicesApproved = true
	p.Spec.Services.Ntfy.Enabled = false
	must(c.Update(ctx, p))
	reconcileOnce()

	stsExists := func(name string) bool {
		var s appsv1.StatefulSet
		return c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: name}, &s) == nil
	}
	markReady := func(name string) {
		t.Helper()
		var s appsv1.StatefulSet
		must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: name}, &s))
		n := *s.Spec.Replicas
		s.Status.Replicas, s.Status.ReadyReplicas, s.Status.AvailableReplicas, s.Status.CurrentReplicas, s.Status.UpdatedReplicas = n, n, n, n, n
		s.Status.ObservedGeneration = s.Generation
		must(c.Status().Update(ctx, &s))
	}
	cond := func(typ string) *metav1.Condition {
		return meta.FindStatusCondition(getPlatform().Status.Conditions, typ)
	}

	if !stsExists("postgres") || stsExists("forgejo") {
		t.Fatal("postgres must be applied first and forgejo wait for it")
	}
	if c := cond(v1alpha1.ConditionForgejoReady); c == nil || c.Reason != "Waiting" {
		t.Fatalf("forgejo condition %+v", c)
	}
	for _, n := range []string{"postgres-superuser", "postgres-forgejo", "postgres-gateway"} {
		if secret(v1alpha1.PlatformNamespace, n) == nil {
			t.Errorf("secret %s missing", n)
		}
	}
	if s := secret(sys, "control-center-db"); s == nil || !strings.HasPrefix(string(s.Data["url"]), "postgres://controlcenter:") {
		t.Error("control-center-db missing or wrong")
	}
	markReady("postgres")
	var jobs batchv1.JobList
	must(c.List(ctx, &jobs, client.InNamespace(v1alpha1.PlatformNamespace)))
	if len(jobs.Items) != 1 {
		t.Fatalf("%d bootstrap jobs", len(jobs.Items))
	}
	job := &jobs.Items[0]
	now := metav1.Now()
	job.Status.StartTime, job.Status.CompletionTime, job.Status.Succeeded = &now, &now, 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	must(c.Status().Update(ctx, job))
	reconcileOnce()
	if !stsExists("forgejo") {
		t.Fatal("forgejo not applied after postgres was ready")
	}

	admin := secret(v1alpha1.PlatformNamespace, "forgejo-admin")
	srv, scopes := fakeForgejo(t, string(admin.Data["username"]), string(admin.Data["password"]))
	r.ForgejoURL = srv.URL
	markReady("forgejo")
	reconcileOnce()
	if tok := string(secret(v1alpha1.PlatformNamespace, "forgejo-admin").Data["token"]); !strings.HasPrefix(tok, "tok-swarmageddon-operator-") {
		t.Fatalf("admin token %q", tok)
	}
	if s := secret(sys, "forge-reader"); s == nil || !strings.HasPrefix(string(s.Data["token"]), "tok-swarmageddon-reader-") {
		t.Fatal("forge-reader missing")
	}
	if strings.Join(*scopes, "|") != "all|read:admin,read:repository,read:organization,read:user" {
		t.Fatalf("scopes %v", *scopes)
	}
	if !stsExists("messaging") || secret(v1alpha1.PlatformNamespace, "messaging-tls") == nil {
		t.Fatal("messaging not applied")
	}
	var msts appsv1.StatefulSet
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "messaging"}, &msts))
	if *msts.Spec.Replicas != 1 {
		t.Fatalf("single node should run 1 messaging replica, got %d", *msts.Spec.Replicas)
	}
	markReady("messaging")
	reconcileOnce()
	var gw appsv1.Deployment
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "gateway"}, &gw))
	gw.Status.Replicas, gw.Status.ReadyReplicas, gw.Status.AvailableReplicas, gw.Status.UpdatedReplicas = 1, 1, 1, 1
	must(c.Status().Update(ctx, &gw))
	reconcileOnce()
	markReady("registry-cache")
	reconcileOnce()
	if rc := cond(v1alpha1.ConditionReady); rc == nil || rc.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition %+v", rc)
	}
	if nc := cond(v1alpha1.ConditionNtfyReady); nc == nil || nc.Reason != "Disabled" {
		t.Fatalf("ntfy condition %+v", nc)
	}
	if v := getPlatform().Status.Versions; v["operator"] != "v0.1.0" || v["forgejo"] != imgs.Forgejo {
		t.Fatalf("versions %v", v)
	}
	var np networkingv1.NetworkPolicyList
	must(c.List(ctx, &np, client.InNamespace(v1alpha1.PlatformNamespace)))
	if len(np.Items) != 7 {
		t.Fatalf("%d platform NetworkPolicies", len(np.Items))
	}
}
