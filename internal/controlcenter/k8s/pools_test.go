package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestNodeDiskLow(t *testing.T) {
	ws := func(n string) *v1alpha1.Workspace { return &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: n}} }
	pool := func(ws string, status metav1.ConditionStatus) *v1alpha1.RunnerPool {
		return &v1alpha1.RunnerPool{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ws-" + ws},
			Status: v1alpha1.RunnerPoolStatus{Conditions: []metav1.Condition{{Type: ConditionPoolDiskLow, Status: status, Reason: "x",
				Message: "most free on a node: 3 GB, minimum 5 GB"}}}}
	}
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(ws("acme"), ws("beta"), ws("gamma"),
		pool("acme", metav1.ConditionTrue), pool("beta", metav1.ConditionFalse)).Build()
	got, err := (&Client{C: c}).NodeDiskLow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["acme"] != "most free on a node: 3 GB, minimum 5 GB" || got["beta"] != "" {
		t.Fatalf("got %q", got)
	}
}
