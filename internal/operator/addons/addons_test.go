package addons

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

func TestNtfyCredentials(t *testing.T) {
	d, err := NtfyCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^tk_[-_A-Za-z0-9]{29}$`).Match(d["token"]) { // ntfy's allowedTokenRegex
		t.Errorf("token %q", d["token"])
	}
	name, hash, ok := strings.Cut(strings.TrimSuffix(string(d["users"]), ":admin"), ":")
	if !ok || name != NtfyAdmin || bcrypt.CompareHashAndPassword([]byte(hash), d["password"]) != nil {
		t.Errorf("users %q", d["users"])
	}
	if cost, _ := bcrypt.Cost([]byte(hash)); cost < 10 {
		t.Errorf("bcrypt cost %d", cost)
	}
	if string(d["tokens"]) != NtfyAdmin+":"+string(d["token"])+":control-center" {
		t.Errorf("tokens %q", d["tokens"])
	}
}

func TestAddons(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, ns := range []string{v1alpha1.SystemNamespace, v1alpha1.PlatformNamespace} {
		must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	must(c.Create(ctx, &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik",
		Annotations: map[string]string{networkingv1.AnnotationIsDefaultIngressClass: "true"}}, Spec: networkingv1.IngressClassSpec{Controller: "traefik.io/ingress-controller"}}))
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}, Spec: v1alpha1.PlatformSpec{Hostname: "swarm.example.com"}}
	p.Spec.Services.Ntfy.Enabled = true
	must(c.Create(ctx, p))

	oc := env.OperatorClient(t)
	tier := "userns"
	r := &Reconciler{Client: oc, Scheme: env.Scheme, Deps: deps.Deps{
		CAStore: &pki.Store{Client: oc, Namespace: v1alpha1.SystemNamespace}, Pinned: images.Defaults("v0.1.0"), Version: "v0.1.0",
		PlatformTier: func(context.Context) (string, error) { return tier, nil },
	}}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: v1alpha1.PlatformName}}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()

	// Device plugin.
	var ds appsv1.DaemonSet
	must(c.Get(ctx, types.NamespacedName{Namespace: manifests.DevicesNamespace, Name: "devices"}, &ds))
	if img := ds.Spec.Template.Spec.Containers[0].Image; !strings.HasSuffix(img, "/devices:v0.1.0") {
		t.Errorf("device plugin image %s", img)
	}
	var dns corev1.Namespace
	must(c.Get(ctx, types.NamespacedName{Name: manifests.DevicesNamespace}, &dns))
	if dns.Labels["pod-security.kubernetes.io/enforce"] != "privileged" {
		t.Errorf("devices namespace labels %v", dns.Labels)
	}

	// ntfy credentials, the same in both Secrets.
	var auth, pub corev1.Secret
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: NtfyAuthSecret}, &auth))
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: NtfyPublisherSecret}, &pub))
	if string(pub.Data["token"]) == "" || string(pub.Data["token"]) != string(auth.Data["token"]) || !strings.HasPrefix(string(pub.Data["url"]), "http://ntfy.") {
		t.Errorf("publisher %v", pub.Data)
	}
	token := string(auth.Data["token"])

	// Forgejo's ingress and certificate.
	var ing networkingv1.Ingress
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "forgejo"}, &ing))
	if ing.Spec.Rules[0].Host != "forge.swarm.example.com" || ing.Spec.TLS[0].SecretName != ForgejoTLSSecret || *ing.Spec.IngressClassName != "traefik" {
		t.Errorf("ingress %+v", ing.Spec)
	}
	var tlsSec corev1.Secret
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: ForgejoTLSSecret}, &tlsSec))
	cert, err := pki.ParseCert(tlsSec.Data[pki.TLSCertKey])
	must(err)
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "forge.swarm.example.com" {
		t.Errorf("certificate names %v", cert.DNSNames)
	}

	// Stable: credentials aren't regenerated.
	reconcile()
	must(c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: NtfyAuthSecret}, &auth))
	if string(auth.Data["token"]) != token {
		t.Error("ntfy token regenerated")
	}

	// Another tier and no hostname: the plugin and the ingress go.
	tier = "privileged"
	must(c.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, p))
	p.Spec.Hostname = ""
	must(c.Update(ctx, p))
	reconcile()
	for _, o := range []client.Object{
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: manifests.DevicesNamespace, Name: "devices"}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.PlatformNamespace, Name: "forgejo"}},
	} {
		err := c.Get(ctx, client.ObjectKeyFromObject(o), o)
		if !apierrors.IsNotFound(err) && o.GetDeletionTimestamp().IsZero() {
			t.Errorf("%T %s still there (%v)", o, o.GetName(), err)
		}
	}
}
