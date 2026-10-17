package platform

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
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

// On a new k3s install the operator waits for k3s's bundled Traefik
// before publishing a NodePort URL, then falls back after the grace period.
func TestAwaitBundledIngress(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	created := time.Date(2026, 10, 17, 5, 0, 0, 0, time.UTC)
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName, CreationTimestamp: metav1.NewTime(created)}}
	node := func(kubelet string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: kubelet}}}
	}
	for _, tc := range []struct {
		name    string
		kubelet string
		after   time.Duration
		wait    bool
	}{
		{"k3s, new", "v1.36.2+k3s1", time.Minute, true},
		{"k3s, after the grace period", "v1.36.2+k3s1", 4 * time.Minute, false},
		{"not k3s", "v1.36.2", time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(node(tc.kubelet)).Build(),
				Deps:   deps.Deps{Clock: clocktesting.NewFakePassiveClock(created.Add(tc.after))},
			}
			d, err := r.awaitBundledIngress(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if (d > 0) != tc.wait {
				t.Errorf("wait = %v, want waiting %v", d, tc.wait)
			}
		})
	}
}
