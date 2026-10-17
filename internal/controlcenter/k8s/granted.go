package k8s

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// The control center may create any Secret or ConfigMap in the system
// namespace but read and change only its own (docs/design.md §11.3). Those
// with a per-provider, per-channel or per-workspace name are granted by the
// operator's Role control-center-owned shortly after they exist, so writes
// create first and then wait for the grant.

// grantWait is how long a Get that is Forbidden is retried: about 3 s.
var grantWait = wait.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Steps: 6}

// whenGranted runs fn, again while it fails with Forbidden.
func whenGranted(fn func() error) error {
	return retry.OnError(grantWait, apierrors.IsForbidden, fn)
}

// ownedLabels mark what the control center creates; the operator grants
// prefixed names only to objects with them.
func ownedLabels() map[string]string {
	return map[string]string{v1alpha1.LabelComponent: "control-center"}
}

func (c *Client) ownedSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: ownedLabels()},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func (c *Client) ownedConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: ownedLabels()},
		Data:       data,
	}
}
