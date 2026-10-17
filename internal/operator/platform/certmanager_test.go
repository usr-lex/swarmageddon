package platform

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

func TestUseTLSSecret(t *testing.T) {
	objs, err := manifests.Render(manifests.ControlCenterIngress, manifests.Values{SystemNamespace: v1alpha1.SystemNamespace,
		Ingress: manifests.IngressValues{ClassName: "traefik", Hostname: "agents.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := useTLSSecret(objs, PublicTLSSecret); err != nil {
		t.Fatal(err)
	}
	tls, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "tls")
	if len(tls) != 1 || tls[0].(map[string]any)["secretName"] != PublicTLSSecret {
		t.Fatalf("tls = %v", tls)
	}
}

// TestCertManagerTLS runs the certManager mode with the operator's RBAC:
// a clear condition without cert-manager, then a Certificate whose Secret
// the ingress uses once cert-manager reports it ready.
func TestCertManagerTLS(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	sys := v1alpha1.SystemNamespace
	if err := env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sys}}); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme, Deps: deps.Deps{Version: "v0.1.0"}}
	waitForRBAC(t, r.Client)
	if err := EnsurePlatform(ctx, env.Client); err != nil {
		t.Fatal(err)
	}
	var p v1alpha1.Platform
	if err := env.Client.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p); err != nil {
		t.Fatal(err)
	}
	check := func(wantSecret, wantReason string) {
		t.Helper()
		secret, _, err := r.publicTLS(ctx, &p)
		if err != nil {
			t.Fatal(err)
		}
		if secret != wantSecret {
			t.Errorf("secret = %s, want %s", secret, wantSecret)
		}
		if wantReason == "" {
			return
		}
		c := meta.FindStatusCondition(p.Status.Conditions, v1alpha1.ConditionControlCenterTLS)
		if c == nil || c.Reason != wantReason {
			t.Errorf("condition = %+v, want reason %s", c, wantReason)
		}
	}

	check(ControlCenterTLSSecret, "")
	p.Spec.TLS = v1alpha1.TLSSpec{Mode: v1alpha1.TLSCertManager, IssuerRef: &v1alpha1.IssuerRef{Name: "letsencrypt"}}
	check(ControlCenterTLSSecret, "NoHostname")
	p.Spec.Hostname = "agents.example.com"
	check(ControlCenterTLSSecret, "CertManagerMissing")

	// Install cert-manager's Certificate kind (schema-less, enough here).
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "certificates.cert-manager.io"},
		"spec": map[string]any{
			"group": "cert-manager.io", "scope": "Namespaced",
			"names": map[string]any{"kind": "Certificate", "listKind": "CertificateList", "plural": "certificates", "singular": "certificate"},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}}}},
		},
	}}
	if err := env.Client.Create(ctx, crd); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !r.hasKind(certificateGVK) {
		if time.Now().After(deadline) {
			t.Fatal("Certificate kind never appeared")
		}
		time.Sleep(200 * time.Millisecond)
	}

	check(ControlCenterTLSSecret, "Issuing")
	cert := newCertificate()
	key := types.NamespacedName{Namespace: sys, Name: certificateName}
	if err := env.Client.Get(ctx, key, cert); err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(cert.Object, "spec")
	issuer := spec["issuerRef"].(map[string]any)
	if spec["secretName"] != PublicTLSSecret || spec["dnsNames"].([]any)[0] != "agents.example.com" ||
		issuer["name"] != "letsencrypt" || issuer["kind"] != "ClusterIssuer" {
		t.Fatalf("certificate spec = %v", spec)
	}

	// cert-manager issues it.
	_ = unstructured.SetNestedSlice(cert.Object, []any{map[string]any{"type": "Ready", "status": "True", "message": "issued"}}, "status", "conditions")
	if err := env.Client.Update(ctx, cert); err != nil {
		t.Fatal(err)
	}
	check(ControlCenterTLSSecret, "Issuing") // no Secret yet
	if err := env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: sys, Name: PublicTLSSecret},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y")}}); err != nil {
		t.Fatal(err)
	}
	check(PublicTLSSecret, "CertManager")

	// Back to our CA: the Certificate goes.
	p.Spec.TLS = v1alpha1.TLSSpec{Mode: v1alpha1.TLSSelfSigned}
	check(ControlCenterTLSSecret, "")
	if err := env.Client.Get(ctx, key, newCertificate()); !apierrors.IsNotFound(err) {
		t.Fatalf("certificate after selfSigned: %v", err)
	}
}
