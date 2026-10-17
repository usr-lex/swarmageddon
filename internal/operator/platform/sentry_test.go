package platform

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
	"github.com/usr-lex/swarmageddon/internal/telemetry"
)

// TestSentrySecret: the operator copies its DSN into the platform namespace,
// replaces it when it changes and removes it when reporting is turned off.
func TestSentrySecret(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	for _, err := range []error{
		c.Create(ctx, p),
		c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformNamespace}}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme}
	key := types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: manifests.SentrySecret}
	dsn := func() string {
		t.Helper()
		var s corev1.Secret
		if err := c.Get(ctx, key, &s); apierrors.IsNotFound(err) {
			return ""
		} else if err != nil {
			t.Fatal(err)
		}
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Name != v1alpha1.PlatformName {
			t.Errorf("owners %v", s.OwnerReferences)
		}
		return string(s.Data["dsn"])
	}

	// Off: nothing to remove, nothing made.
	if err := r.syncSentrySecret(ctx, p); err != nil || dsn() != "" {
		t.Fatal(err)
	}
	for _, d := range []string{"https://a@o1.ingest.example/1", "https://b@o1.ingest.example/2"} {
		r.Deps.Sentry = telemetry.Config{DSN: d}
		if err := r.syncSentrySecret(ctx, p); err != nil {
			t.Fatal(err)
		}
		if got := dsn(); got != d {
			t.Fatalf("dsn %q, want %q", got, d)
		}
	}
	r.Deps.Sentry = telemetry.Config{}
	if err := r.syncSentrySecret(ctx, p); err != nil || dsn() != "" {
		t.Fatalf("not removed: %v", err)
	}
}

func TestSentryValues(t *testing.T) {
	if v := sentryValues(telemetry.Config{TracesSampleRate: 0.1}); v != (manifests.SentryValues{}) {
		t.Fatalf("off: %+v", v)
	}
	v := sentryValues(telemetry.Config{DSN: "https://a@o1.ingest.example/1", Environment: "prod", TracesSampleRate: 0.1})
	if v.Hash == "" || v.Environment != "prod" || v.TracesSampleRate != "0.1" {
		t.Fatalf("%+v", v)
	}
	if v2 := sentryValues(telemetry.Config{DSN: "https://a@o1.ingest.example/2"}); v2.Hash == v.Hash || v2.TracesSampleRate != "0" {
		t.Fatalf("%+v", v2)
	}
}
