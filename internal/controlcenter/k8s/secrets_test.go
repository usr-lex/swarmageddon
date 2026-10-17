package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestSecretAllowlist(t *testing.T) {
	for _, n := range []string{SecretSetupToken, SecretLocalAdmin, SecretSession, SecretDB, SecretVAPID, SecretSAML,
		SecretGateway, SecretForgeReader, "ntfy-publisher", SecretForgeWebhookPrefix + "acme", "gateway-provider-anthropic",
		"oidc-google", "notify-acme-slack"} {
		if !AllowedSecret(n) {
			t.Errorf("%s not allowed", n)
		}
	}
	// The operator's own Secrets, and the control center's certificates
	// (mounted, never read through the API).
	for _, n := range []string{"platform-ca", "control-center-tls", "control-center-client", "forgejo-admin",
		"gateway-master", "ntfy-auth", "notify-", "oidc", "room-secrets"} {
		if AllowedSecret(n) {
			t.Errorf("%s allowed", n)
		}
	}
	if !v1alpha1.ControlCenterConfigMap("saml-okta") || !v1alpha1.ControlCenterConfigMap("platform-ca-bundle") ||
		v1alpha1.ControlCenterConfigMap("messaging-revoked") || v1alpha1.ControlCenterConfigMap("saml-") {
		t.Error("ConfigMap allowlist")
	}
}

// forbiddenGets makes the first n Gets of name fail with Forbidden, as they
// do until the operator grants a new name.
func forbiddenGets(n int, name string) (interceptor.Funcs, *int) {
	calls := 0
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if key.Name == name {
			calls++
			if calls <= n {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, name, errors.New("not yet"))
			}
		}
		return c.Get(ctx, key, obj, opts...)
	}}, &calls
}

func newClient(t *testing.T, f interceptor.Funcs, objs ...client.Object) (*Client, client.Client) {
	t.Helper()
	old := grantWait
	grantWait = wait.Backoff{Duration: time.Millisecond, Factor: 2, Steps: 5}
	t.Cleanup(func() { grantWait = old })
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(objs...).WithInterceptorFuncs(f).Build()
	return &Client{C: c, Namespace: v1alpha1.SystemNamespace}, c
}

func TestApplySecretCreatesLabelled(t *testing.T) {
	ctx := context.Background()
	kc, c := newClient(t, interceptor.Funcs{})
	if err := kc.ApplySecret(ctx, "oidc-google", map[string][]byte{"clientSecret": []byte("a")}); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "oidc-google"}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Labels[v1alpha1.LabelComponent] != "control-center" || string(s.Data["clientSecret"]) != "a" {
		t.Fatalf("secret %+v", s)
	}
	if err := kc.ApplySecret(ctx, "platform-ca", nil); !errors.Is(err, ErrSecretNotAllowed) {
		t.Fatalf("platform-ca: %v", err)
	}
}

func TestApplySecretWaitsForTheGrant(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oidc-google", Namespace: v1alpha1.SystemNamespace},
		Data: map[string][]byte{"clientSecret": []byte("old")}}
	f, calls := forbiddenGets(2, "oidc-google")
	kc, c := newClient(t, f, existing)
	if err := kc.ApplySecret(ctx, "oidc-google", map[string][]byte{"clientSecret": []byte("new")}); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: "oidc-google"}, &s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["clientSecret"]) != "new" || *calls < 3 {
		t.Fatalf("data %q after %d gets", s.Data["clientSecret"], *calls)
	}
}

func TestApplySecretGivesUpWhenNeverGranted(t *testing.T) {
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "notify-acme-slack", Namespace: v1alpha1.SystemNamespace}}
	f, calls := forbiddenGets(1000, "notify-acme-slack")
	kc, _ := newClient(t, f, existing)
	err := kc.ApplySecret(context.Background(), "notify-acme-slack", map[string][]byte{"url": []byte("x")})
	if !apierrors.IsForbidden(err) || *calls != 5 {
		t.Fatalf("err %v after %d gets", err, *calls)
	}
}

func TestCreateSecretIfMissingReadsTheStoredOne(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretSAML, Namespace: v1alpha1.SystemNamespace},
		Data: map[string][]byte{"key": []byte("stored")}}
	f, _ := forbiddenGets(1, SecretSAML)
	kc, _ := newClient(t, f, existing)
	d, err := kc.CreateSecretIfMissing(ctx, SecretSAML, map[string][]byte{"key": []byte("mine")})
	if err != nil || string(d["key"]) != "stored" {
		t.Fatalf("data %v err %v", d, err)
	}
	d, err = kc.CreateSecretIfMissing(ctx, SecretVAPID, map[string][]byte{"publicKey": []byte("p")})
	if err != nil || string(d["publicKey"]) != "p" {
		t.Fatalf("new: data %v err %v", d, err)
	}
}

func TestConfigMaps(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "saml-okta", Namespace: v1alpha1.SystemNamespace},
		Data: map[string]string{"metadata.xml": "old"}}
	f, _ := forbiddenGets(1, "saml-okta")
	kc, _ := newClient(t, f, existing)
	if err := kc.ApplyConfigMap(ctx, "saml-okta", map[string]string{"metadata.xml": "new"}); err != nil {
		t.Fatal(err)
	}
	d, err := kc.GetConfigMap(ctx, "saml-okta")
	if err != nil || d["metadata.xml"] != "new" {
		t.Fatalf("data %v err %v", d, err)
	}
	if err := kc.ApplyConfigMap(ctx, "saml-azure", map[string]string{"metadata.xml": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := kc.ApplyConfigMap(ctx, "platform-ca-bundle", nil); err == nil {
		t.Fatal("wrote platform-ca-bundle")
	}
	if _, err := kc.GetConfigMap(ctx, "messaging-revoked"); !errors.Is(err, ErrConfigMapNotAllowed) {
		t.Fatalf("messaging-revoked: %v", err)
	}
}
