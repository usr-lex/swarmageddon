package platform

import (
	"context"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
)

// limitControlCenterIngress applies NetworkPolicy control-center (design
// §11.1): the default IngressClass's controller and Forgejo's webhooks
// reach the control center, or any address when it is a NodePort.
func (r *Reconciler) limitControlCenterIngress(ctx context.Context, p *v1alpha1.Platform) error {
	var classes networkingv1.IngressClassList
	if err := r.List(ctx, &classes); err != nil {
		return err
	}
	v := manifests.Values{Namespace: v1alpha1.PlatformNamespace, SystemNamespace: v1alpha1.SystemNamespace}
	for _, ic := range classes.Items {
		if ic.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true" {
			v.Ingress = manifests.IngressValues{ClassName: ic.Name, ControllerPods: manifests.IngressControllerPods(ic.Spec.Controller)}
		}
	}
	return r.render(ctx, p, manifests.ControlCenterNetworkPolicy, v)
}
