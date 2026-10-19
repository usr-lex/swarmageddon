package room

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// A claim that hasn't bound yet (WaitForFirstConsumer: its pod isn't
// scheduled) has no capacity; it will be provisioned at its requested size,
// so it isn't "expanding" (CI runs 25 and 28 reported "expanding home to
// 2Gi" for rooms whose pods were never scheduled).
func TestUnboundClaimIsNotExpanding(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	size := resource.MustParse("2Gi")
	room := &v1.Room{ObjectMeta: metav1.ObjectMeta{Namespace: "ws-acme", Name: "athena"},
		Spec: v1.RoomSpec{Volumes: v1.RoomVolumes{Home: &size, Engine: &size}}}
	var objs []runtime.Object
	for _, vol := range []string{"home", "engine"} {
		objs = append(objs, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ws-acme", Name: VolumeClaimName("athena", vol)},
			Spec:       corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}}},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		})
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(objs...).Build()}
	st := &v1.RoomStatus{}
	if err := r.resizeVolumes(context.Background(), room, st); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(st.Conditions, ConditionVolumesReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("VolumesReady %+v", c)
	}
}
