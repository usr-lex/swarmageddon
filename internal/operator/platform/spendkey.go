package platform

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// ControlCenterGatewaySecret holds the control center's gateway key for
// reading spend (design §10.2), key masterKey (the name the control center
// reads; the key is view-only, not LiteLLM's master key).
const ControlCenterGatewaySecret = "control-center-gateway"

// controlCenterGatewayUser is the LiteLLM user behind that key.
const controlCenterGatewayUser = "control-center"

// ensureControlCenterGatewayKey issues the control center's key once
// LiteLLM is up: a key of a proxy_admin_viewer user, which lists every key
// with its spend but can't change anything or call a model. Failures are
// retried on the next reconcile; until then the control center shows no spend.
func (r *Reconciler) ensureControlCenterGatewayKey(ctx context.Context, p *v1alpha1.Platform) {
	nn := types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: ControlCenterGatewaySecret}
	if err := r.Get(ctx, nn, &corev1.Secret{}); !apierrors.IsNotFound(err) || r.Deps.Gateway == nil {
		return
	}
	gc, err := r.Deps.Gateway(ctx)
	if err != nil {
		return
	}
	k, err := gc.ViewerKey(ctx, controlCenterGatewayUser, fmt.Sprintf("control-center-spend-%d", r.now().Unix()))
	if err != nil {
		log.FromContext(ctx).Error(err, "control center gateway key")
		return
	}
	if _, err := r.ensureSecret(ctx, p, nn, "control-center", staticData(map[string]string{"masterKey": k.Key})); err != nil {
		log.FromContext(ctx).Error(err, "control center gateway key")
	}
}
