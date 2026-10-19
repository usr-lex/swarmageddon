// Package addons reconciles the platform pieces that sit next to the
// platform services: the device plugin for the userns tier (ADR 0001),
// ntfy's credentials (design §10.3), and Forgejo's ingress at
// forge.<hostname> (§9.6). It watches the Platform like the platform
// controller, and applies only these objects.
package addons

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"golang.org/x/crypto/bcrypt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/apply"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/openshift"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// Secrets for ntfy (docs/conventions.md).
const (
	// NtfyAuthSecret in the platform namespace feeds ntfy's declarative
	// users and tokens (keys users, tokens, username, password, token).
	NtfyAuthSecret = "ntfy-auth"
	// NtfyPublisherSecret in the system namespace is what the control center
	// publishes with (keys token, username, password, url).
	NtfyPublisherSecret = "ntfy-publisher"
	// NtfyAdmin is the provisioned ntfy user.
	NtfyAdmin = "swarm-admin"
	// ForgejoTLSSecret holds forge.<hostname>'s certificate.
	ForgejoTLSSecret = "forgejo-tls"
	// ForgejoPublicTLSSecret is cert-manager's certificate for
	// forge.<hostname>, in the certManager TLS mode.
	ForgejoPublicTLSSecret = "forgejo-public-tls"
)

// Reconciler applies the add-ons for the Platform.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Deps   deps.Deps
}

// +kubebuilder:rbac:groups=swarmageddon.dev,resources=platforms,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingressclasses,verbs=get;list;watch

// SetupWithManager registers the add-ons controller.
func SetupWithManager(mgr ctrl.Manager, d deps.Deps) error {
	r := &Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Deps: d}
	toPlatform := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: v1alpha1.PlatformName}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("addons").
		For(&v1alpha1.Platform{}).
		Watches(&networkingv1.IngressClass{}, toPlatform).
		Watches(&appsv1.DaemonSet{}, toPlatform).
		Complete(r)
}

func (r *Reconciler) applier() *apply.Applier {
	return &apply.Applier{Client: r.Client, Scheme: r.Scheme, Version: r.Deps.Version}
}

// Reconcile applies each add-on; one failing doesn't stop the others.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != v1alpha1.PlatformName {
		return ctrl.Result{}, nil
	}
	var p v1alpha1.Platform
	if err := r.Get(ctx, req.NamespacedName, &p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !p.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	errs := []error{r.devices(ctx, &p), r.ntfy(ctx, &p), r.forgeIngress(ctx, &p)}
	// An add-on waiting for a platform service (or the tier) isn't an
	// error: look again soon instead of retrying in a tight backoff.
	waiting := false
	for i, err := range errs {
		if errors.Is(err, deps.ErrNotReady) {
			log.FromContext(ctx).V(1).Info("add-on waiting", "reason", err.Error())
			errs[i], waiting = nil, true
		}
	}
	if err := errors.Join(errs...); err != nil {
		return ctrl.Result{}, err
	}
	if waiting {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	// Certificates renew well before they expire; look again daily.
	return ctrl.Result{RequeueAfter: 24 * time.Hour}, nil
}

func (r *Reconciler) images() images.Images {
	if r.Deps.Pinned.Devices == "" {
		return images.Defaults(r.Deps.Version)
	}
	return r.Deps.Pinned
}

// devices runs the device plugin while rooms use the userns tier.
func (r *Reconciler) devices(ctx context.Context, p *v1alpha1.Platform) error {
	tier := string(v1alpha1.TierUserns)
	if r.Deps.PlatformTier != nil {
		t, err := r.Deps.PlatformTier(ctx)
		if err != nil {
			return err
		}
		if t != "" && t != string(v1alpha1.TierAuto) {
			tier = t
		}
	}
	if tier != string(v1alpha1.TierUserns) {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: manifests.DevicesNamespace, Name: "devices"}}
		return client.IgnoreNotFound(r.Delete(ctx, ds))
	}
	dv := manifests.DevicesValues{Namespace: manifests.DevicesNamespace}
	if r.Deps.OpenShift {
		dv.SELinuxType = openshift.DevicePluginSELinuxType
	}
	objs, err := manifests.Render(manifests.Devices, manifests.Values{Images: r.images(), Devices: dv})
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := r.applier().Apply(ctx, p, o); err != nil {
			return err
		}
	}
	if r.Deps.OpenShift {
		return r.applier().Apply(ctx, p, openshift.Binding(manifests.DevicesNamespace, openshift.Devices))
	}
	return nil
}

// ntfy provisions ntfy's admin with a password and a token, once, when
// ntfy is enabled: ntfy denies everyone else (auth-default-access deny-all).
func (r *Reconciler) ntfy(ctx context.Context, p *v1alpha1.Platform) error {
	if !p.Spec.Services.Ntfy.Enabled {
		return nil
	}
	auth := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: NtfyAuthSecret}, auth)
	if apierrors.IsNotFound(err) {
		data, err := NtfyCredentials()
		if err != nil {
			return err
		}
		auth = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.PlatformNamespace, Name: NtfyAuthSecret,
			Labels: map[string]string{v1alpha1.LabelManaged: "true", v1alpha1.LabelComponent: "ntfy"}}, Type: corev1.SecretTypeOpaque, Data: data}
		if err := controllerutil.SetControllerReference(p, auth, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, auth); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	pub := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: NtfyPublisherSecret}, pub)
	if !apierrors.IsNotFound(err) {
		return err
	}
	pub = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.SystemNamespace, Name: NtfyPublisherSecret,
		Labels: map[string]string{v1alpha1.LabelManaged: "true", v1alpha1.LabelComponent: "ntfy"}}, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"token": auth.Data["token"], "username": auth.Data["username"], "password": auth.Data["password"],
			"url": []byte("http://ntfy." + v1alpha1.PlatformNamespace + ".svc"),
		}}
	if err := controllerutil.SetControllerReference(p, pub, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pub)
}

// NtfyCredentials makes the ntfy-auth Secret's data: an admin with a
// random password (stored bcrypt-hashed for ntfy) and a token, as ntfy's
// auth-users ("name:hash:role") and auth-tokens ("name:token:label").
func NtfyCredentials() (map[string][]byte, error) {
	password, err := randomString(32)
	if err != nil {
		return nil, err
	}
	tail, err := randomString(29)
	if err != nil {
		return nil, err
	}
	token := "tk_" + tail
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 10) // ntfy requires cost >= 10
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"username": []byte(NtfyAdmin),
		"password": []byte(password),
		"token":    []byte(token),
		"users":    []byte(NtfyAdmin + ":" + string(hash) + ":admin"),
		"tokens":   []byte(NtfyAdmin + ":" + token + ":control-center"),
	}, nil
}

// randomString is n characters of [a-z0-9] (ntfy tokens are lowercase).
func randomString(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[k.Int64()]
	}
	return string(b), nil
}

// forgeIngress publishes Forgejo at forge.<hostname> through the default
// IngressClass, with a certificate from the platform CA, as the control
// center is. Without a hostname or an IngressClass it removes the Ingress.
func (r *Reconciler) forgeIngress(ctx context.Context, p *v1alpha1.Platform) error {
	ns := v1alpha1.PlatformNamespace
	class, err := defaultIngressClass(ctx, r.Client)
	if err != nil {
		return err
	}
	if p.Spec.Hostname == "" || class == "" {
		for _, o := range []client.Object{
			&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "forgejo"}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "forgejo-ingress"}},
		} {
			if err := r.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
	if r.Deps.CAStore == nil {
		return errors.New("no CA store")
	}
	ca, err := r.Deps.CAStore.CA(ctx)
	if err != nil {
		return err
	}
	host := "forge." + p.Spec.Hostname
	if _, err := pki.EnsureTLSSecret(ctx, r.Client, ca, types.NamespacedName{Namespace: ns, Name: ForgejoTLSSecret},
		pki.Request{CommonName: host, DNSNames: []string{host}}, r.Deps.Now(), func(s *corev1.Secret) error {
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			s.Labels[v1alpha1.LabelManaged] = "true"
			s.Labels[v1alpha1.LabelComponent] = "forgejo"
			return controllerutil.SetControllerReference(p, s, r.Scheme)
		}); err != nil {
		return fmt.Errorf("forgejo certificate: %w", err)
	}
	secret, waiting := r.forgeTLSSecret(ctx, p, host)
	if waiting != nil && !errors.Is(waiting, deps.ErrNotReady) {
		return waiting
	}
	objs, err := manifests.Render(manifests.ForgejoIngress, manifests.Values{Namespace: ns,
		Ingress: manifests.IngressValues{ClassName: class, Hostname: host, TLSSecret: secret}})
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := r.applier().Apply(ctx, p, o); err != nil {
			return err
		}
	}
	return waiting
}

var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

func forgeCertificate() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(certificateGVK)
	u.SetNamespace(v1alpha1.PlatformNamespace)
	u.SetName("forgejo")
	return u
}

// forgeTLSSecret is the Secret forge.<hostname>'s ingress serves. In the
// certManager TLS mode it keeps a cert-manager Certificate for the host from
// spec.tls.issuerRef, like the control center's, and switches to it once it
// is issued (until then it returns the platform CA's Secret and an error
// wrapping deps.ErrNotReady, to look again soon). In every other case, or
// without cert-manager, it is the platform CA's.
func (r *Reconciler) forgeTLSSecret(ctx context.Context, p *v1alpha1.Platform, host string) (string, error) {
	_, err := r.RESTMapper().RESTMapping(certificateGVK.GroupKind(), certificateGVK.Version)
	installed := err == nil
	issuer := p.Spec.TLS.IssuerRef
	if p.Spec.TLS.Mode != v1alpha1.TLSCertManager || issuer == nil || issuer.Name == "" || !installed {
		if installed {
			if err := r.Delete(ctx, forgeCertificate()); client.IgnoreNotFound(err) != nil {
				return ForgejoTLSSecret, err
			}
		}
		return ForgejoTLSSecret, nil
	}
	kind := issuer.Kind
	if kind == "" {
		kind = "ClusterIssuer"
	}
	cert := forgeCertificate()
	cert.Object["spec"] = map[string]any{
		"secretName": ForgejoPublicTLSSecret,
		"dnsNames":   []any{host},
		"issuerRef":  map[string]any{"group": "cert-manager.io", "kind": kind, "name": issuer.Name},
	}
	cert.SetLabels(map[string]string{v1alpha1.LabelComponent: "forgejo"})
	if err := r.applier().Apply(ctx, p, cert); err != nil {
		return ForgejoTLSSecret, err
	}
	cur := forgeCertificate()
	if err := r.Get(ctx, client.ObjectKeyFromObject(cur), cur); err != nil {
		return ForgejoTLSSecret, err
	}
	conds, _, _ := unstructured.NestedSlice(cur.Object, "status", "conditions")
	ready := false
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" && m["status"] == "True" {
			ready = true
		}
	}
	if !ready {
		return ForgejoTLSSecret, fmt.Errorf("%w: cert-manager is issuing forge.%s's certificate", deps.ErrNotReady, p.Spec.Hostname)
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: ForgejoPublicTLSSecret}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return ForgejoTLSSecret, fmt.Errorf("%w: Secret %s not written yet", deps.ErrNotReady, ForgejoPublicTLSSecret)
		}
		return ForgejoTLSSecret, err
	}
	return ForgejoPublicTLSSecret, nil
}

func defaultIngressClass(ctx context.Context, c client.Client) (string, error) {
	var classes networkingv1.IngressClassList
	if err := c.List(ctx, &classes); err != nil {
		return "", err
	}
	for _, ic := range classes.Items {
		if ic.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true" {
			return ic.Name, nil
		}
	}
	return "", nil
}
