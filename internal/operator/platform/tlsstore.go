package platform

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
)

// +kubebuilder:rbac:groups=traefik.io,resources=tlsstores,verbs=get;list;watch;create;update;patch;delete

var tlsStoreGVK = schema.GroupVersionKind{Group: "traefik.io", Version: "v1alpha1", Kind: "TLSStore"}

// traefikDefaultTLSStore is the only TLSStore name Traefik uses.
const traefikDefaultTLSStore = "default"

// traefikDefaultCert makes control-center-tls Traefik's default
// certificate, which it serves to clients that send no SNI, such as a
// browser opening https://<node-ip>/ (design §3.2). Traefik uses one
// TLSStore named default in the whole cluster, so when another namespace
// has one, that installation keeps it (and ours goes, so there aren't two)
// and the returned note, for the ControlCenterExposed condition, says what
// IP access shows instead.
func (r *Reconciler) traefikDefaultCert(ctx context.Context, p *v1alpha1.Platform) (string, error) {
	if !r.hasKind(tlsStoreGVK) {
		return "", nil
	}
	var stores unstructured.UnstructuredList
	stores.SetGroupVersionKind(tlsStoreGVK.GroupVersion().WithKind("TLSStoreList"))
	if err := r.List(ctx, &stores); err != nil {
		return "", err
	}
	for _, s := range stores.Items {
		if s.GetName() == traefikDefaultTLSStore && s.GetNamespace() != v1alpha1.SystemNamespace {
			if err := r.removeTraefikDefaultCert(ctx); err != nil {
				return "", err
			}
			return fmt.Sprintf("; Traefik's default certificate is TLSStore %s/default's, so https://<node-ip>/ shows that one: use a hostname or the port-forward",
				s.GetNamespace()), nil
		}
	}
	return "", r.render(ctx, p, manifests.ControlCenterTLSStore, manifests.Values{SystemNamespace: v1alpha1.SystemNamespace})
}

// removeTraefikDefaultCert deletes our TLSStore, if there is one.
func (r *Reconciler) removeTraefikDefaultCert(ctx context.Context) error {
	if !r.hasKind(tlsStoreGVK) {
		return nil
	}
	s := &unstructured.Unstructured{}
	s.SetGroupVersionKind(tlsStoreGVK)
	s.SetNamespace(v1alpha1.SystemNamespace)
	s.SetName(traefikDefaultTLSStore)
	if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
