// Package platform reconciles the singleton Platform (docs/design.md §3, §5):
// namespaces, the internal CA and the control center's certificates and
// keys, the setup token, the control center's exposure, preflight, and the
// platform services in order once the wizard approves them.
package platform

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/apply"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/preflight"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// Annotations and condition types beyond the API package's.
const (
	// AnnotationRerunPreflight asks the operator to run preflight again
	// whenever its value (the control center writes a timestamp) differs
	// from status.preflightRerun.
	AnnotationRerunPreflight = "swarmageddon.dev/rerun-preflight"
	// ConditionSetupToken reports where the setup token is while admin is unclaimed.
	ConditionSetupToken = "SetupToken"
	// ConditionExposed reports how the control center is reached.
	ConditionExposed = "ControlCenterExposed"

	SetupTokenSecret = "setup-token"
)

// Reconciler reconciles the Platform named "default".
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Deps   deps.Deps
	// Preflight runs the cluster checks.
	Preflight *preflight.Runner
	// HTTPClient talks to Forgejo; nil uses a client with a 30 s timeout.
	HTTPClient *http.Client
	// ForgejoURL overrides http://forgejo.swarmageddon-platform.svc:3000 (tests).
	ForgejoURL string
	// Out receives the WELCOME block; nil is stdout.
	Out io.Writer
}

// +kubebuilder:rbac:groups=swarmageddon.dev,resources=platforms;workspaces;rooms;roletemplates;runnerpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=swarmageddon.dev,resources=platforms/status;workspaces/status;rooms/status;runnerpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=swarmageddon.dev,resources=platforms/finalizers;workspaces/finalizers;rooms/finalizers;runnerpools/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;secrets;configmaps;services;serviceaccounts;persistentvolumeclaims;pods;resourcequotas;limitranges,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/exec;pods/portforward,verbs=create
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete;deletecollection
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingressclasses;servicecidrs,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=traefik.io,resources=serverstransports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager registers the controller and a runnable that creates the
// Platform "default" if it is missing.
func SetupWithManager(mgr ctrl.Manager, d deps.Deps, pf *preflight.Runner) error {
	r := &Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Deps: d, Preflight: pf}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return EnsurePlatform(ctx, mgr.GetClient())
	})); err != nil {
		return err
	}
	toPlatform := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: v1alpha1.PlatformName}}}
	})
	inNamespaces := func(nss ...string) predicate.Predicate {
		return predicate.NewPredicateFuncs(func(o client.Object) bool { return slices.Contains(nss, o.GetNamespace()) })
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("platform").
		For(&v1alpha1.Platform{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&batchv1.Job{}).
		Watches(&corev1.Pod{}, toPlatform, builder.WithPredicates(inNamespaces(preflight.Namespace))).
		Watches(&corev1.Secret{}, toPlatform, builder.WithPredicates(inNamespaces(v1alpha1.SystemNamespace))).
		Watches(&corev1.Service{}, toPlatform, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetNamespace() == v1alpha1.SystemNamespace && o.GetName() == "control-center"
		}))).
		Watches(&networkingv1.IngressClass{}, toPlatform).
		Complete(r)
}

// EnsurePlatform creates the singleton Platform if there is none.
func EnsurePlatform(ctx context.Context, c client.Client) error {
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	if err := c.Create(ctx, p); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create platform: %w", err)
	}
	return nil
}

func (r *Reconciler) now() time.Time {
	return r.Deps.Now()
}

func (r *Reconciler) applier() *apply.Applier {
	return &apply.Applier{Client: r.Client, Scheme: r.Scheme, Version: r.Deps.Version}
}

func (r *Reconciler) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (r *Reconciler) forgejoURL() string {
	if r.ForgejoURL != "" {
		return r.ForgejoURL
	}
	return "http://forgejo." + v1alpha1.PlatformNamespace + ".svc:3000"
}

func (r *Reconciler) ownedBy(p *v1alpha1.Platform, component string) func(*corev1.Secret) error {
	return func(s *corev1.Secret) error {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[v1alpha1.LabelManaged] = "true"
		s.Labels[v1alpha1.LabelComponent] = component
		return controllerutil.SetControllerReference(p, s, r.Scheme)
	}
}

func setCond(p *v1alpha1.Platform, typ string, ok bool, reason, msg string) {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: p.Generation})
}

// Reconcile drives the Platform one step further.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != v1alpha1.PlatformName {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	var p v1alpha1.Platform
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := p.DeepCopy()
	result, err := r.reconcile(ctx, logger, &p)
	p.Status.ObservedGeneration = p.Generation
	if perr := r.Status().Patch(ctx, &p, client.MergeFrom(orig)); perr != nil && err == nil {
		err = perr
	}
	return result, err
}

func (r *Reconciler) reconcile(ctx context.Context, logger logr.Logger, p *v1alpha1.Platform) (ctrl.Result, error) {
	// Renew certificates well before 2/3 of a 90-day life; requeue at least twice a day.
	result := ctrl.Result{RequeueAfter: 12 * time.Hour}
	requeue := func(d time.Duration) {
		if d < result.RequeueAfter {
			result.RequeueAfter = d
		}
	}

	if err := r.applier().Apply(ctx, p, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformNamespace, Labels: map[string]string{
			"pod-security.kubernetes.io/enforce": "baseline",
		}},
	}); err != nil {
		return result, err
	}

	ca, err := r.Deps.CAStore.CA(ctx)
	if err != nil {
		return result, fmt.Errorf("platform CA: %w", err)
	}
	p.Status.CAFingerprint = ca.Fingerprint()
	for _, ns := range []string{v1alpha1.SystemNamespace, v1alpha1.PlatformNamespace} {
		if err := r.applier().Apply(ctx, p, CABundle(ns, ca)); err != nil {
			return result, err
		}
	}

	if err := r.ensureControlCenterSecrets(ctx, p, ca); err != nil {
		setCond(p, v1alpha1.ConditionControlCenterTLS, false, "Error", err.Error())
		return result, err
	}
	setCond(p, v1alpha1.ConditionControlCenterTLS, true, "Issued", "control-center-tls and control-center-client are current")

	if err := r.expose(ctx, p); err != nil {
		setCond(p, ConditionExposed, false, "Error", err.Error())
		return result, err
	}

	if err := r.setupToken(ctx, logger, p); err != nil {
		return result, err
	}

	if d, err := r.preflight(ctx, p); err != nil {
		return result, err
	} else if d > 0 {
		requeue(d)
	}

	p.Status.Versions = r.Deps.Pinned.Versions(r.Deps.Version)

	if !p.Spec.Wizard.ServicesApproved {
		for _, s := range services {
			setCond(p, s.condition, false, "NotApproved", "waiting for the wizard to approve the platform services")
		}
		setCond(p, v1alpha1.ConditionReady, false, "NotApproved", "platform services not approved yet")
		return result, nil
	}
	v, err := r.values(ctx, p)
	if err != nil {
		return result, err
	}
	if err := r.render(ctx, p, manifests.NetworkPolicies, v); err != nil {
		return result, err
	}
	allReady, blocked := true, ""
	for _, s := range services {
		if s.enabled != nil && !s.enabled(p) {
			setCond(p, s.condition, false, "Disabled", "not enabled")
			continue
		}
		if blocked != "" {
			setCond(p, s.condition, false, "Waiting", "waiting for "+blocked)
			allReady = false
			continue
		}
		ready, msg, err := s.apply(r, ctx, p, v)
		if err != nil {
			setCond(p, s.condition, false, "Error", err.Error())
			return result, err
		}
		if !ready {
			setCond(p, s.condition, false, "Progressing", msg)
			blocked, allReady = s.condition, false
			requeue(10 * time.Second)
			continue
		}
		setCond(p, s.condition, true, "Ready", msg)
	}
	if allReady {
		setCond(p, v1alpha1.ConditionReady, true, "Ready", "all platform services are ready")
	} else {
		setCond(p, v1alpha1.ConditionReady, false, "Progressing", "waiting for "+blocked)
	}
	return result, nil
}

// CABundle is the platform-ca-bundle ConfigMap for namespace ns.
func CABundle(ns string, ca *pki.CA) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-ca-bundle", Namespace: ns},
		Data:       map[string]string{pki.CACertKey: string(ca.CertPEM)},
	}
}

func (r *Reconciler) ensureControlCenterSecrets(ctx context.Context, p *v1alpha1.Platform, ca *pki.CA) error {
	sys := v1alpha1.SystemNamespace
	ips, err := r.nodeIPs(ctx)
	if err != nil {
		return err
	}
	serving := pki.Request{
		CommonName: "control-center",
		DNSNames:   []string{"control-center", "control-center." + sys, "control-center." + sys + ".svc", "control-center." + sys + ".svc.cluster.local", "localhost"},
		IPs:        append([]net.IP{net.ParseIP("127.0.0.1")}, ips...),
	}
	if p.Spec.Hostname != "" {
		serving.DNSNames = append(serving.DNSNames, p.Spec.Hostname)
	}
	if _, err := pki.EnsureTLSSecret(ctx, r.Client, ca, types.NamespacedName{Namespace: sys, Name: "control-center-tls"}, serving, r.now(), r.ownedBy(p, "control-center")); err != nil {
		return err
	}
	clientReq := pki.Request{CommonName: "control-center", URIs: []string{"spiffe://swarmageddon/system/control-center"}, Usage: pki.Client}
	if _, err := pki.EnsureTLSSecret(ctx, r.Client, ca, types.NamespacedName{Namespace: sys, Name: "control-center-client"}, clientReq, r.now(), r.ownedBy(p, "control-center")); err != nil {
		return err
	}
	if _, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: sys, Name: "control-center-session"}, "control-center",
		func() (map[string][]byte, error) { return map[string][]byte{"key": randomBytes(32)}, nil }); err != nil {
		return err
	}
	if _, err := r.ensureSecret(ctx, p, types.NamespacedName{Namespace: sys, Name: "vapid"}, "control-center", vapidKeys); err != nil {
		return err
	}
	return nil
}

// nodeIPs returns the nodes' internal and external IPs, sorted.
func (r *Reconciler) nodeIPs(ctx context.Context) ([]net.IP, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, err
	}
	var addrs []string
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				addrs = append(addrs, a.Address)
			}
		}
	}
	slices.Sort(addrs)
	var out []net.IP
	for _, a := range slices.Compact(addrs) {
		if ip := net.ParseIP(a); ip != nil {
			out = append(out, ip)
		}
	}
	return out, nil
}

// expose publishes the control center through the default IngressClass or,
// without one, a NodePort (design §3.2), and records status.url.
func (r *Reconciler) expose(ctx context.Context, p *v1alpha1.Platform) error {
	sys := v1alpha1.SystemNamespace
	var classes networkingv1.IngressClassList
	if err := r.List(ctx, &classes); err != nil {
		return err
	}
	class := ""
	for _, ic := range classes.Items {
		if ic.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true" {
			class = ic.Name
		}
	}
	ips, err := r.nodeIPs(ctx)
	if err != nil {
		return err
	}
	host := "127.0.0.1"
	if len(ips) > 0 {
		host = ips[0].String()
	}
	if p.Spec.Hostname != "" {
		host = p.Spec.Hostname
	}
	var svc corev1.Service
	svcErr := r.Get(ctx, types.NamespacedName{Namespace: sys, Name: "control-center"}, &svc)
	if svcErr != nil && !apierrors.IsNotFound(svcErr) {
		return svcErr
	}
	portForward := "kubectl -n " + sys + " port-forward svc/control-center 8443:443, then https://localhost:8443/"
	v := manifests.Values{SystemNamespace: sys, Ingress: manifests.IngressValues{ClassName: class, Hostname: p.Spec.Hostname}}

	if class != "" {
		objs, err := manifests.Render(manifests.ControlCenterIngress, v)
		if err != nil {
			return err
		}
		if err := r.applier().ApplyAll(ctx, p, toObjects(objs)); err != nil {
			return err
		}
		if r.hasKind(schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "ServersTransport"}) && svcErr == nil {
			objs, err := manifests.Render(manifests.ControlCenterTraefik, v)
			if err != nil {
				return err
			}
			for _, o := range objs {
				owner := client.Object(p)
				if o.GetKind() == "Service" {
					owner = nil // the install owns the Service; we only add annotations
				}
				if err := r.applier().Apply(ctx, owner, o); err != nil {
					return err
				}
			}
		}
		p.Status.URL = "https://" + host + "/"
		setCond(p, ConditionExposed, true, "Ingress", "IngressClass "+class+"; or "+portForward)
		return nil
	}

	// No default IngressClass: drop our Ingress and use a NodePort.
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: sys, Name: "control-center"}}
	if err := r.Delete(ctx, ing); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if svcErr != nil {
		p.Status.URL = "https://localhost:8443/"
		setCond(p, ConditionExposed, false, "NoService", "Service control-center not found; is the control center installed?")
		return nil
	}
	if err := r.applier().Apply(ctx, nil, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: sys, Name: "control-center"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort},
	}); err != nil {
		return err
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sys, Name: "control-center"}, &svc); err != nil {
		return err
	}
	var port int32
	for _, sp := range svc.Spec.Ports {
		if sp.Port == 443 || port == 0 {
			port = sp.NodePort
		}
	}
	if port == 0 {
		p.Status.URL = "https://localhost:8443/"
		setCond(p, ConditionExposed, false, "NodePortPending", "waiting for a node port; meanwhile "+portForward)
		return nil
	}
	p.Status.URL = fmt.Sprintf("https://%s:%d/", host, port)
	setCond(p, ConditionExposed, true, "NodePort", fmt.Sprintf("node port %d (no default IngressClass); or %s", port, portForward))
	return nil
}

func toObjects[T client.Object](in []T) []client.Object {
	out := make([]client.Object, len(in))
	for i, o := range in {
		out[i] = o
	}
	return out
}

func (r *Reconciler) hasKind(gvk schema.GroupVersionKind) bool {
	_, err := r.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil
}

// setupToken keeps Secret setup-token while admin is unclaimed (design §3.3).
// The control center sets status.adminClaimed; the operator then deletes the
// token. The reset annotation issues a new token and clears adminClaimed.
func (r *Reconciler) setupToken(ctx context.Context, logger logr.Logger, p *v1alpha1.Platform) error {
	key := types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: SetupTokenSecret}
	_, reset := p.Annotations[v1alpha1.AnnotationResetSetupToken]
	if reset {
		if err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		p.Status.AdminClaimed = false
		logger.Info("setup token reset requested; issuing a new one")
	}
	if p.Status.AdminClaimed {
		meta.RemoveStatusCondition(&p.Status.Conditions, ConditionSetupToken)
		err := r.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}})
		return client.IgnoreNotFound(err)
	}
	var sec corev1.Secret
	err := r.Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		token := NewSetupToken()
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{
				v1alpha1.LabelManaged: "true", v1alpha1.LabelComponent: "control-center",
			}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"token": []byte(token)},
		}
		if err := controllerutil.SetControllerReference(p, &sec, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, &sec); err != nil {
			return err
		}
		r.welcome(p, token)
	} else if err != nil {
		return err
	}
	setCond(p, ConditionSetupToken, true, "Issued", "Secret "+key.Namespace+"/"+key.Name+", key token")
	if reset {
		return r.removeAnnotation(ctx, p, v1alpha1.AnnotationResetSetupToken)
	}
	return nil
}

// removeAnnotation patches an annotation away without touching the
// in-memory status, which the caller patches at the end.
func (r *Reconciler) removeAnnotation(ctx context.Context, p *v1alpha1.Platform, key string) error {
	cp := p.DeepCopy()
	patch := client.MergeFrom(cp.DeepCopy())
	delete(cp.Annotations, key)
	if err := r.Patch(ctx, cp, patch); err != nil {
		return err
	}
	delete(p.Annotations, key)
	return nil
}

// welcome prints the setup token once, in a block that stands out in the log.
func (r *Reconciler) welcome(p *v1alpha1.Platform, token string) {
	out := r.Out
	if out == nil {
		out = os.Stdout
	}
	url := p.Status.URL
	if url == "" {
		url = "https://localhost:8443/ (kubectl -n " + v1alpha1.SystemNamespace + " port-forward svc/control-center 8443:443)"
	}
	fmt.Fprintf(out, `
==================== swarmageddon: WELCOME ====================
Open the control center:  %s
Port-forward fallback:    kubectl -n %s port-forward svc/control-center 8443:443
CA fingerprint (SHA-256): %s
Setup token:              %s
The token is printed once. It is also in Secret %s/%s (key token).
===============================================================

`, url, v1alpha1.SystemNamespace, p.Status.CAFingerprint, token, v1alpha1.SystemNamespace, SetupTokenSecret)
}

// preflight runs the checks until done. It returns how soon to look again.
func (r *Reconciler) preflight(ctx context.Context, p *v1alpha1.Platform) (time.Duration, error) {
	if want := p.Annotations[AnnotationRerunPreflight]; want != "" && want != p.Status.PreflightRerun {
		if err := r.Preflight.Cleanup(ctx); err != nil {
			return 0, err
		}
		p.Status.Preflight = nil
		p.Status.PreflightRerun = want
		// Let the old probe pods go away before starting again.
		setCond(p, v1alpha1.ConditionPreflightDone, false, "Restarting", "re-running preflight")
		return 5 * time.Second, nil
	}
	if p.Status.Preflight != nil && p.Status.Preflight.CompletedAt != nil {
		return 0, nil
	}
	res, err := r.Preflight.Run(ctx, &p.Spec)
	if err != nil {
		setCond(p, v1alpha1.ConditionPreflightDone, false, "Error", err.Error())
		return 0, err
	}
	p.Status.Preflight = res.Status
	if !res.Done {
		setCond(p, v1alpha1.ConditionPreflightDone, false, "Running", "probes running")
		return 5 * time.Second, nil
	}
	now := metav1.NewTime(r.now())
	p.Status.Preflight.CompletedAt = &now
	failed := 0
	for _, c := range res.Status.Checks {
		if c.Result == v1alpha1.CheckFail {
			failed++
		}
	}
	msg := fmt.Sprintf("tier %q, about %d rooms, %d failed checks", res.Status.Tier, res.Status.RoomEstimate, failed)
	setCond(p, v1alpha1.ConditionPreflightDone, true, "Completed", msg)
	return 0, r.Preflight.Cleanup(ctx)
}
