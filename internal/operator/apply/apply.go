// Package apply does server-side apply under the field manager
// "swarmageddon" (docs/design.md §5). Every applied object gets an owner
// reference to its owner (if any) and the swarmageddon.dev/version label.
package apply

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// FieldManager is the server-side apply field manager of everything the
// operator writes.
const FieldManager = "swarmageddon"

// Applier applies objects for one owner.
type Applier struct {
	Client  client.Client
	Scheme  *runtime.Scheme
	Version string
}

// Apply server-side applies obj (typed or unstructured), with force, so the
// operator wins conflicts on the fields it sets. If owner is not nil, it
// becomes obj's controller owner reference. On return obj is not updated;
// read it back if you need server state.
func (a *Applier) Apply(ctx context.Context, owner client.Object, obj client.Object) error {
	u, err := a.ToUnstructured(obj)
	if err != nil {
		return err
	}
	if owner != nil {
		if err := controllerutil.SetControllerReference(owner, u, a.Scheme); err != nil {
			return fmt.Errorf("owner reference on %s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[v1alpha1.LabelManaged] = "true"
	if a.Version != "" {
		labels[v1alpha1.LabelVersion] = sanitizeLabel(a.Version)
	}
	u.SetLabels(labels)
	u.SetManagedFields(nil)
	u.SetResourceVersion("")
	u.SetUID("")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "status")
	if err := a.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
	}
	return nil
}

// ApplyAll applies objs in order and stops at the first error.
func (a *Applier) ApplyAll(ctx context.Context, owner client.Object, objs []client.Object) error {
	for _, o := range objs {
		if err := a.Apply(ctx, owner, o); err != nil {
			return err
		}
	}
	return nil
}

// ToUnstructured converts obj, filling in its kind from the scheme if needed.
func (a *Applier) ToUnstructured(obj client.Object) (*unstructured.Unstructured, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.DeepCopy(), nil
	}
	gvk, err := apiutil.GVKForObject(obj, a.Scheme)
	if err != nil {
		return nil, err
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetGroupVersionKind(gvk)
	return u, nil
}

// sanitizeLabel makes a version string a valid label value (63 chars of
// [A-Za-z0-9._-], alphanumeric at both ends).
func sanitizeLabel(v string) string {
	b := []byte(v)
	for i, c := range b {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			b[i] = '_'
		}
	}
	if len(b) > 63 {
		b = b[:63]
	}
	for len(b) > 0 && !isAlnum(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && !isAlnum(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
