package openshift

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

// The operator, with exactly its generated ClusterRole, may bind each SCC
// role: Kubernetes refuses a RoleBinding to a role the creator can't bind.
func TestOperatorMayBindTheSCCRoles(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	oc := env.OperatorClient(t)
	for i, role := range []string{Nonroot, Rooms, Devices} {
		ns := []string{"swarmageddon-platform", "ws-acme", "swarmageddon-devices"}[i]
		if err := env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
		b := Binding(ns, role)
		if err := oc.Create(ctx, b); err != nil {
			t.Fatalf("bind %s in %s: %v", role, ns, err)
		}
		if s := b.Subjects[0]; s.Kind != "Group" || s.Name != "system:serviceaccounts:"+ns {
			t.Errorf("subjects %+v", b.Subjects)
		}
	}
}
