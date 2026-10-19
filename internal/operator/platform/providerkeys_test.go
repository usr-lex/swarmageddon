package platform

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

// TestProviderKeys: a key the control center stores in the system namespace
// reaches the gateway's namespace, follows changes, and goes away with its
// provider. A Secret an admin made in the platform namespace is left alone.
func TestProviderKeys(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	sys, plat := v1alpha1.SystemNamespace, v1alpha1.PlatformNamespace
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	must(c.Create(ctx, p))
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sys}}))
	must(c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: plat}}))
	putKey := func(ns, name, key string) {
		t.Helper()
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: map[string][]byte{"apiKey": []byte(key)}}
		if err := c.Create(ctx, s); apierrors.IsAlreadyExists(err) {
			must(c.Update(ctx, s))
		} else {
			must(err)
		}
	}
	key := func(name string) (string, bool) {
		t.Helper()
		var s corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: plat, Name: name}, &s)
		if apierrors.IsNotFound(err) {
			return "", false
		}
		must(err)
		return string(s.Data["apiKey"]), true
	}
	ref := func(name string) *corev1.LocalObjectReference { return &corev1.LocalObjectReference{Name: name} }

	putKey(sys, "gateway-provider-litellm", "sk-one")
	putKey(plat, "admin-made", "sk-admin")
	p.Spec.Services.Gateway.Providers = []v1alpha1.ModelProvider{
		{Name: "litellm", Kind: v1alpha1.ProviderOpenAICompatible, APIKeySecretRef: ref("gateway-provider-litellm"), Models: []string{"m"}},
		{Name: "local", Kind: v1alpha1.ProviderOpenAICompatible, Models: []string{"l"}},
		{Name: "byhand", Kind: v1alpha1.ProviderAnthropic, APIKeySecretRef: ref("admin-made"), Models: []string{"c"}},
	}
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme}

	h1, err := r.syncProviderKeys(ctx, p)
	must(err)
	if k, ok := key("gateway-provider-litellm"); !ok || k != "sk-one" {
		t.Fatalf("copied key %q %v", k, ok)
	}
	if k, _ := key("admin-made"); k != "sk-admin" {
		t.Fatalf("admin's Secret changed: %q", k)
	}

	// A new key in the wizard: the copy follows and the hash changes, so
	// LiteLLM restarts with it.
	putKey(sys, "gateway-provider-litellm", "sk-two")
	h2, err := r.syncProviderKeys(ctx, p)
	must(err)
	if k, _ := key("gateway-provider-litellm"); k != "sk-two" {
		t.Fatalf("key not updated: %q", k)
	}
	if h1 == h2 {
		t.Fatal("hash did not change with the key")
	}
	if h3, _ := r.syncProviderKeys(ctx, p); h3 != h2 {
		t.Fatal("hash not stable")
	}

	// The provider is removed: its copy goes, the admin's Secret stays.
	p.Spec.Services.Gateway.Providers = p.Spec.Services.Gateway.Providers[1:]
	_, err = r.syncProviderKeys(ctx, p)
	must(err)
	if _, ok := key("gateway-provider-litellm"); ok {
		t.Fatal("copy kept after its provider was removed")
	}
	if _, ok := key("admin-made"); !ok {
		t.Fatal("admin's Secret deleted")
	}
}
