package platform

import (
	"context"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
)

// providerKeyComponent labels the copies syncProviderKeys makes.
const providerKeyComponent = "gateway-provider"

// syncProviderKeys copies each provider's API key Secret from the system
// namespace, where the control center writes it, to the platform namespace,
// where LiteLLM reads it. A copy follows its source and goes away with its
// provider; a Secret that only exists in the platform namespace (made by an
// admin) is used as it is. It returns a hash of the keys for LiteLLM's pod
// template, so a new key restarts it.
func (r *Reconciler) syncProviderKeys(ctx context.Context, p *v1alpha1.Platform) (string, error) {
	var want, hashed []string
	for _, pr := range p.Spec.Services.Gateway.Providers {
		if pr.APIKeySecretRef == nil {
			continue
		}
		name := pr.APIKeySecretRef.Name
		var src corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: name}, &src)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		want = append(want, name)
		hashed = append(hashed, name+"="+string(src.Data["apiKey"]))
		if err := r.applier().Apply(ctx, p, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: v1alpha1.PlatformNamespace,
				Labels: map[string]string{v1alpha1.LabelComponent: providerKeyComponent}},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"apiKey": src.Data["apiKey"]},
		}); err != nil {
			return "", err
		}
	}
	var copies corev1.SecretList
	if err := r.List(ctx, &copies, client.InNamespace(v1alpha1.PlatformNamespace),
		client.MatchingLabels{v1alpha1.LabelComponent: providerKeyComponent}); err != nil {
		return "", err
	}
	for i := range copies.Items {
		if !slices.Contains(want, copies.Items[i].Name) {
			if err := r.Delete(ctx, &copies.Items[i]); client.IgnoreNotFound(err) != nil {
				return "", err
			}
		}
	}
	if len(hashed) == 0 {
		return "", nil
	}
	return manifests.Hash(strings.Join(hashed, "\n")), nil
}
