package addons

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

// In the certManager TLS mode forge.<hostname> gets a cert-manager
// certificate too, not only the control center; the platform CA's serves
// until it is issued.
func TestForgeCertManagerTLS(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	if err := env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformNamespace}}); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme, Deps: deps.Deps{Version: "v0.1.0"}}
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}, Spec: v1alpha1.PlatformSpec{Hostname: "agents.example.com",
		TLS: v1alpha1.TLSSpec{Mode: v1alpha1.TLSCertManager, IssuerRef: &v1alpha1.IssuerRef{Name: "letsencrypt"}}}}
	if err := env.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	// Without cert-manager: the platform CA's certificate, no error.
	if s, err := r.forgeTLSSecret(ctx, p, "forge.agents.example.com"); err != nil || s != ForgejoTLSSecret {
		t.Fatalf("no cert-manager: %q, %v", s, err)
	}
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
	var s string
	var err error
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err = r.forgeTLSSecret(ctx, p, "forge.agents.example.com")
		if errors.Is(err, deps.ErrNotReady) || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !errors.Is(err, deps.ErrNotReady) || s != ForgejoTLSSecret {
		t.Fatalf("issuing: %q, %v", s, err)
	}
	cert := forgeCertificate()
	key := types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "forgejo"}
	if err := env.Client.Get(ctx, key, cert); err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(cert.Object, "spec")
	if spec["secretName"] != ForgejoPublicTLSSecret || spec["dnsNames"].([]any)[0] != "forge.agents.example.com" {
		t.Fatalf("certificate spec %v", spec)
	}
	_ = unstructured.SetNestedSlice(cert.Object, []any{map[string]any{"type": "Ready", "status": "True"}}, "status", "conditions")
	if err := env.Client.Update(ctx, cert); err != nil {
		t.Fatal(err)
	}
	if err := env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.PlatformNamespace, Name: ForgejoPublicTLSSecret},
		Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y")}}); err != nil {
		t.Fatal(err)
	}
	if s, err := r.forgeTLSSecret(ctx, p, "forge.agents.example.com"); err != nil || s != ForgejoPublicTLSSecret {
		t.Fatalf("issued: %q, %v", s, err)
	}
	// Back to the platform CA: the Certificate goes.
	p.Spec.TLS = v1alpha1.TLSSpec{Mode: v1alpha1.TLSSelfSigned}
	if s, err := r.forgeTLSSecret(ctx, p, "forge.agents.example.com"); err != nil || s != ForgejoTLSSecret {
		t.Fatalf("selfSigned: %q, %v", s, err)
	}
	if err := env.Client.Get(ctx, key, forgeCertificate()); err == nil {
		t.Fatal("certificate kept after selfSigned")
	}
}
