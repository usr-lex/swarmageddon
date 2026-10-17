package platform

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// service is one platform service step, applied in order (design §3.4).
type service struct {
	condition string
	enabled   func(*v1alpha1.Platform) bool
	// optional services don't hold back the ones after them while not
	// ready: the registry cache can't start where Docker Hub is out of
	// reach (an air-gapped cluster), and rooms work without it.
	optional bool
	// apply renders and applies the service and reports whether it is ready.
	apply func(r *Reconciler, ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (ready bool, msg string, err error)
}

var services = []service{
	{condition: v1alpha1.ConditionPostgresReady, apply: (*Reconciler).applyPostgres},
	{condition: v1alpha1.ConditionForgejoReady, apply: (*Reconciler).applyForgejo},
	{condition: v1alpha1.ConditionMessagingReady, apply: (*Reconciler).applyMessaging},
	{condition: v1alpha1.ConditionGatewayReady, apply: (*Reconciler).applyGateway},
	{condition: ConditionEgressReady, apply: (*Reconciler).applyEgress},
	{condition: v1alpha1.ConditionRegistryReady, apply: (*Reconciler).applyRegistry, optional: true},
	{condition: v1alpha1.ConditionNtfyReady, apply: (*Reconciler).applyNtfy,
		enabled: func(p *v1alpha1.Platform) bool { return p.Spec.Services.Ntfy.Enabled }},
}

func storage(q *resource.Quantity, def string) string {
	if q != nil && !q.IsZero() {
		return q.String()
	}
	return def
}

// values computes the template values from the Platform.
func (r *Reconciler) values(ctx context.Context, p *v1alpha1.Platform) (manifests.Values, error) {
	ns := v1alpha1.PlatformNamespace
	s := p.Spec.Services
	v := manifests.Values{Namespace: ns, SystemNamespace: v1alpha1.SystemNamespace, Images: r.Deps.Pinned}

	var dbs []manifests.Database
	var names []string
	for _, d := range pgDatabases {
		dbs = append(dbs, manifests.Database{Name: d.name, Secret: d.secret})
		names = append(names, d.name+"="+d.secret)
	}
	v.Postgres = manifests.PostgresValues{Storage: storage(s.Postgres.Storage, "20Gi"), Databases: dbs,
		BootstrapHash: manifests.Hash(strings.Join(names, ",") + r.Deps.Pinned.Postgres)}

	v.Forgejo = manifests.ForgejoValues{
		Storage:   storage(s.Forgejo.Storage, "50Gi"),
		RootURL:   fmt.Sprintf("http://forgejo.%s.svc:3000/", ns),
		Domain:    fmt.Sprintf("forgejo.%s.svc", ns),
		SSHDomain: fmt.Sprintf("forgejo-ssh.%s.svc", ns),
	}
	if p.Spec.Hostname != "" {
		v.Forgejo.Domain = "forge." + p.Spec.Hostname
		v.Forgejo.RootURL = "https://forge." + p.Spec.Hostname + "/"
	}

	replicas := s.Messaging.Replicas
	if replicas == 0 {
		var nodes corev1.NodeList
		if err := r.List(ctx, &nodes); err != nil {
			return v, err
		}
		replicas = 3
		if len(nodes.Items) <= 1 {
			replicas = 1
		}
	}
	var routes []string
	for i := range replicas {
		routes = append(routes, fmt.Sprintf("nats://messaging-%d.messaging-headless.%s.svc.cluster.local:6222", i, ns))
	}
	v.Messaging = manifests.MessagingValues{Replicas: replicas, Storage: storage(s.Messaging.Storage, "10Gi"), Routes: strings.Join(routes, ",")}

	cfg, provs := manifests.LiteLLMConfig(s.Gateway.Providers)
	v.Gateway = manifests.GatewayValues{Config: cfg, ConfigHash: manifests.Hash(cfg), Providers: provs}

	v.Registry = manifests.RegistryValues{Storage: storage(s.RegistryCache.Storage, "100Gi"), RemoteURL: "https://registry-1.docker.io", TTL: "168h"}

	v.Ntfy = manifests.NtfyValues{Storage: storage(s.Ntfy.Storage, "1Gi"), BaseURL: "http://ntfy." + ns + ".svc"}
	if p.Spec.Hostname != "" {
		v.Ntfy.BaseURL = "https://ntfy." + p.Spec.Hostname
	}
	v.Sentry = sentryValues(r.Deps.Sentry)
	return v, nil
}

func (r *Reconciler) render(ctx context.Context, p *v1alpha1.Platform, name string, v manifests.Values) error {
	objs, err := manifests.Render(name, v)
	if err != nil {
		return fmt.Errorf("render %s: %w", name, err)
	}
	for _, o := range objs {
		if err := r.applier().Apply(ctx, p, o); err != nil {
			return err
		}
	}
	return nil
}

// statefulSetReady reports whether at least min replicas are ready.
func (r *Reconciler) statefulSetReady(ctx context.Context, name string, min int32) (bool, string, error) {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: name}, &sts); err != nil {
		return false, "", err
	}
	msg := fmt.Sprintf("%d/%d replicas ready", sts.Status.ReadyReplicas, ptrOr(sts.Spec.Replicas, 1))
	return sts.Status.ReadyReplicas >= min, msg, nil
}

func ptrOr(p *int32, d int32) int32 {
	if p == nil {
		return d
	}
	return *p
}

func (r *Reconciler) applyPostgres(ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (bool, string, error) {
	if err := r.ensureDatabaseSecrets(ctx, p); err != nil {
		return false, "", err
	}
	if err := r.render(ctx, p, manifests.Postgres, v); err != nil {
		return false, "", err
	}
	ready, msg, err := r.statefulSetReady(ctx, "postgres", 1)
	if err != nil || !ready {
		return false, msg, err
	}
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "postgres-bootstrap-" + v.Postgres.BootstrapHash}, &job); err != nil {
		if apierrors.IsNotFound(err) {
			// TTL removed a finished Job; re-applying recreates and reruns it (idempotent).
			return false, "bootstrap job recreated", nil
		}
		return false, "", err
	}
	if job.Status.Succeeded < 1 {
		return false, "creating roles and databases", nil
	}
	return true, "databases for " + dbNames(), nil
}

func dbNames() string {
	var n []string
	for _, d := range pgDatabases {
		n = append(n, d.name)
	}
	sort.Strings(n)
	return strings.Join(n, ", ")
}

func (r *Reconciler) applyForgejo(ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (bool, string, error) {
	ns := v1alpha1.PlatformNamespace
	admin, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: ns, Name: "forgejo-admin"}, "forgejo",
		staticData(map[string]string{"username": "swarm-admin", "password": randomHex(24)}))
	if err != nil {
		return false, "", err
	}
	if _, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: ns, Name: "forgejo-secrets"}, "forgejo",
		staticData(map[string]string{"secretKey": randomHex(32)})); err != nil {
		return false, "", err
	}
	if err := r.render(ctx, p, manifests.Forgejo, v); err != nil {
		return false, "", err
	}
	ready, msg, err := r.statefulSetReady(ctx, "forgejo", 1)
	if err != nil || !ready {
		return false, msg, err
	}
	user, pass := string(admin.Data["username"]), string(admin.Data["password"])
	if len(admin.Data["token"]) == 0 {
		tok, err := createForgejoToken(ctx, r.httpClient(), r.forgejoURL(), user, pass, "swarmageddon-operator", operatorTokenScopes, r.now())
		if err != nil {
			return false, "creating the admin token: " + err.Error(), nil
		}
		admin.Data["token"] = []byte(tok)
		if err := r.Update(ctx, admin); err != nil {
			return false, "", err
		}
	}
	// A read-only token for the control center's repository access checks (§9.7).
	var reader corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "forge-reader"}, &reader)
	if apierrors.IsNotFound(err) {
		tok, err := createForgejoToken(ctx, r.httpClient(), r.forgejoURL(), user, pass, "swarmageddon-reader", readerTokenScopes, r.now())
		if err != nil {
			return false, "creating the reader token: " + err.Error(), nil
		}
		if _, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "forge-reader"}, "forgejo",
			staticData(map[string]string{"token": tok, "url": r.forgejoURL()})); err != nil {
			return false, "", err
		}
	} else if err != nil {
		return false, "", err
	}
	return true, "Forgejo is up; admin " + user, nil
}

func (r *Reconciler) applyMessaging(ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (bool, string, error) {
	ca, err := r.Deps.CAStore.CA(ctx)
	if err != nil {
		return false, "", err
	}
	ns := v1alpha1.PlatformNamespace
	req := pki.Request{
		CommonName: "messaging",
		DNSNames: []string{"messaging", "messaging." + ns + ".svc", "messaging." + ns + ".svc.cluster.local",
			"*.messaging-headless." + ns + ".svc.cluster.local", "*.messaging-headless." + ns + ".svc"},
		Usage: pki.ServingAndClient,
	}
	if _, err := pki.EnsureTLSSecret(ctx, r.Client, ca, types.NamespacedName{Namespace: ns, Name: "messaging-tls"}, req, r.now(), r.ownedBy(p, "messaging")); err != nil {
		return false, "", err
	}
	if err := r.render(ctx, p, manifests.Messaging, v); err != nil {
		return false, "", err
	}
	return r.statefulSetReady(ctx, "messaging", v.Messaging.Replicas/2+1)
}

// ConditionEgressReady reports the egress proxy (design §11.2).
const ConditionEgressReady = "EgressReady"

// wardenReviewBudgetUSD is the monthly budget of the gateway warden's
// review key (ADR 0011).
const wardenReviewBudgetUSD = 100

func (r *Reconciler) deploymentAvailable(ctx context.Context, name string) (int32, int32, error) {
	var d appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: name}, &d); err != nil {
		return 0, 0, err
	}
	return d.Status.AvailableReplicas, ptrOr(d.Spec.Replicas, 1), nil
}

// applyGateway deploys LiteLLM (Service litellm) and the warden in front of
// it (Service gateway, ADR 0011).
func (r *Reconciler) applyGateway(ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (bool, string, error) {
	ns := v1alpha1.PlatformNamespace
	if _, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: ns, Name: "gateway-master"}, "gateway",
		staticData(map[string]string{"masterKey": "sk-" + randomHex(24)})); err != nil {
		return false, "", err
	}
	ca, err := r.Deps.CAStore.CA(ctx)
	if err != nil {
		return false, "", err
	}
	req := pki.Request{CommonName: "warden", URIs: []string{"spiffe://swarmageddon/platform/warden"}, Usage: pki.Client}
	if _, err := pki.EnsureTLSSecret(ctx, r.Client, ca, types.NamespacedName{Namespace: ns, Name: "warden-client"}, req, r.now(), r.ownedBy(p, "warden")); err != nil {
		return false, "", err
	}
	if err := r.render(ctx, p, manifests.Gateway, v); err != nil {
		return false, "", err
	}
	// Before the warden, LiteLLM ran as Deployment gateway.
	old := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: "gateway"}, old); err == nil {
		if err := r.Delete(ctx, old); err != nil && !apierrors.IsNotFound(err) {
			return false, "", err
		}
	}
	lAvail, lWant, err := r.deploymentAvailable(ctx, "litellm")
	if err != nil {
		return false, "", err
	}
	wAvail, wWant, err := r.deploymentAvailable(ctx, "warden")
	if err != nil {
		return false, "", err
	}
	msg := fmt.Sprintf("LiteLLM %d/%d, warden %d/%d replicas available; %d providers", lAvail, lWant, wAvail, wWant, len(v.Gateway.Providers))
	ready := lAvail >= 1 && wAvail >= 1
	if ready {
		r.ensureWardenReviewKey(ctx, p)
		r.ensureControlCenterGatewayKey(ctx, p)
	}
	return ready, msg, nil
}

// ensureWardenReviewKey issues the gateway warden's own key for its model
// reviews (Secret warden-review, key "key"), once LiteLLM is up. Failures are
// retried on the next reconcile; until then the warden runs rules only.
func (r *Reconciler) ensureWardenReviewKey(ctx context.Context, p *v1alpha1.Platform) {
	nn := types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "warden-review"}
	if err := r.Get(ctx, nn, &corev1.Secret{}); !apierrors.IsNotFound(err) || r.Deps.Gateway == nil {
		return
	}
	gc, err := r.Deps.Gateway(ctx)
	if err != nil {
		return
	}
	b := float64(wardenReviewBudgetUSD)
	k, err := gc.GenerateKey(ctx, gateway.KeyRequest{KeyAlias: fmt.Sprintf("warden-review-%d", r.now().Unix()), MaxBudget: &b,
		BudgetDuration: "1mo", Metadata: map[string]string{"purpose": "warden"}})
	if err != nil {
		return
	}
	_, _ = r.ensureSecret(ctx, p, nn, "warden", staticData(map[string]string{"key": k.Key}))
}

func (r *Reconciler) applyEgress(ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (bool, string, error) {
	if err := r.render(ctx, p, manifests.Egress, v); err != nil {
		return false, "", err
	}
	avail, want, err := r.deploymentAvailable(ctx, "egress")
	if err != nil {
		return false, "", err
	}
	return avail >= 1, fmt.Sprintf("%d/%d replicas available", avail, want), nil
}

func (r *Reconciler) applyRegistry(ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (bool, string, error) {
	if err := r.render(ctx, p, manifests.RegistryCache, v); err != nil {
		return false, "", err
	}
	return r.statefulSetReady(ctx, "registry-cache", 1)
}

func (r *Reconciler) applyNtfy(ctx context.Context, p *v1alpha1.Platform, v manifests.Values) (bool, string, error) {
	if err := r.render(ctx, p, manifests.Ntfy, v); err != nil {
		return false, "", err
	}
	return r.statefulSetReady(ctx, "ntfy", 1)
}
