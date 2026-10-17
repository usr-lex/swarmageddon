package platform

import (
	"context"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/telemetry"
)

// sentryValues passes the operator's own Sentry settings to the services
// that report (docs/conventions.md); without a DSN they get none.
func sentryValues(c telemetry.Config) manifests.SentryValues {
	if c.DSN == "" {
		return manifests.SentryValues{}
	}
	return manifests.SentryValues{Hash: manifests.Hash(c.DSN), Environment: c.Environment,
		TracesSampleRate: strconv.FormatFloat(c.TracesSampleRate, 'g', -1, 64)}
}

// syncSentrySecret copies the operator's DSN into Secret sentry in the
// platform namespace, which the services' SENTRY_DSN refers to, or deletes
// it when reporting is off.
func (r *Reconciler) syncSentrySecret(ctx context.Context, p *v1alpha1.Platform) error {
	key := types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: manifests.SentrySecret}
	dsn := r.Deps.Sentry.DSN
	if dsn == "" {
		var s corev1.Secret
		if err := r.Get(ctx, key, &s); err != nil {
			return client.IgnoreNotFound(err)
		}
		return client.IgnoreNotFound(r.Delete(ctx, &s))
	}
	return r.applier().Apply(ctx, p, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{v1alpha1.LabelComponent: "sentry"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"dsn": []byte(dsn)},
	})
}
