package platform

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

func tlsStore(ns string) *unstructured.Unstructured {
	s := &unstructured.Unstructured{}
	s.SetGroupVersionKind(tlsStoreGVK)
	s.SetNamespace(ns)
	s.SetName("default")
	return s
}

func TestTraefikDefaultCert(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	if err := c.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme}

	// Without Traefik's CRDs there is nothing to do.
	if note, err := r.traefikDefaultCert(ctx, p); note != "" || err != nil {
		t.Fatalf("no CRD: %q %v", note, err)
	}

	if _, err := envtest.InstallCRDs(env.Config, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{{
		ObjectMeta: metav1.ObjectMeta{Name: "tlsstores.traefik.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "traefik.io", Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "tlsstores", Singular: "tlsstore", Kind: "TLSStore", ListKind: "TLSStoreList"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: ptr.To(true)}}}},
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{v1alpha1.SystemNamespace, "edge"} {
		if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	// Ours: the control center's certificate for clients without SNI.
	if note, err := r.traefikDefaultCert(ctx, p); note != "" || err != nil {
		t.Fatalf("ours: %q %v", note, err)
	}
	ours := tlsStore(v1alpha1.SystemNamespace)
	if err := c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "default"}, ours); err != nil {
		t.Fatal(err)
	}
	if name, _, _ := unstructured.NestedString(ours.Object, "spec", "defaultCertificate", "secretName"); name != "control-center-tls" {
		t.Fatalf("default certificate %q", name)
	}

	// Another installation's default wins, and ours goes so there's one.
	if err := c.Create(ctx, tlsStore("edge")); err != nil {
		t.Fatal(err)
	}
	note, err := r.traefikDefaultCert(ctx, p)
	if err != nil || !strings.Contains(note, "edge/default") || !strings.Contains(note, "port-forward") {
		t.Fatalf("other default: %q %v", note, err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "default"}, tlsStore("")); !apierrors.IsNotFound(err) {
		t.Fatalf("ours kept: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "edge", Name: "default"}, tlsStore("")); err != nil {
		t.Fatalf("theirs: %v", err)
	}
}
