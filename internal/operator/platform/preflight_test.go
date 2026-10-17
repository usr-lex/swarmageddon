package platform

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// A reconcile that still sees the cached Platform from before preflight
// completed must not run preflight again (it would recreate probe pods).
func TestPreflightNotRerunFromStaleCache(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)
	stale := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	fresh := stale.DeepCopy()
	now := metav1.Now()
	fresh.Status.Preflight = &v1alpha1.PreflightStatus{CompletedAt: &now}

	r := &Reconciler{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale.DeepCopy()).Build(),
		APIReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(fresh).Build(),
		Scheme:    scheme,
		// Preflight is nil: running it would panic.
	}
	d, err := r.preflight(context.Background(), stale)
	if err != nil || d != 0 {
		t.Fatalf("preflight() = %v, %v; want 0, nil", d, err)
	}
	if stale.Status.Preflight != nil {
		t.Errorf("status changed from the stale copy: %+v", stale.Status.Preflight)
	}
}
