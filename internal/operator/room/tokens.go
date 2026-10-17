package room

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/tokenhash"
)

// tokenKeys are the identity Secret keys holding tokens the operator issues
// for a room: messaging refuses messages that contain one (design §11.4).
// The VNC password is too short to look for, and keys and certificates
// aren't tokens.
var tokenKeys = []string{KeyForgeToken, KeyGatewayKey, KeyAdvisorKey, KeyWardenKey, KeyEgressAuth}

// syncTokenHashes rewrites the workspace's messaging-token-hashes ConfigMap
// from the identity Secrets of its rooms. cur, if set, stands in for its
// room's Secret (the cache may not have the latest yet); gone names a room
// whose tokens are left out because it is being removed, and then the
// ConfigMap is only updated, never created. Every room recomputes the whole
// set, so the last write is complete, and a changed identity Secret
// requeues its room, which writes it again.
func (r *Reconciler) syncTokenHashes(ctx context.Context, ns string, cur *corev1.Secret, gone string) error {
	var list corev1.SecretList
	if err := r.List(ctx, &list, client.InNamespace(ns), client.MatchingLabels{v1.LabelComponent: ComponentRoom}); err != nil {
		return err
	}
	var tokens []string
	add := func(s *corev1.Secret) {
		for _, k := range tokenKeys {
			tokens = append(tokens, string(s.Data[k]))
		}
	}
	for i := range list.Items {
		s := &list.Items[i]
		room := s.Labels[v1.LabelRoom]
		if s.Name != IdentitySecretName(room) || room == gone || (cur != nil && s.Name == cur.Name) {
			continue
		}
		add(s)
	}
	if cur != nil {
		add(cur)
	}
	want := tokenhash.Format(tokens)

	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: tokenhash.ConfigMapName}, cm)
	if apierrors.IsNotFound(err) && cur == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: tokenhash.ConfigMapName, Namespace: ns,
				Labels: map[string]string{v1.LabelManaged: "true", v1.LabelComponent: "messaging"}},
			Data: map[string]string{tokenhash.DataKey: want},
		}
		if err := r.Create(ctx, cm); !apierrors.IsAlreadyExists(err) {
			return err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if v, ok := cm.Data[tokenhash.DataKey]; ok && v == want {
		return nil
	}
	cm.Data = map[string]string{tokenhash.DataKey: want}
	return r.Update(ctx, cm)
}
