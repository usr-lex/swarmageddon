package room

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// ConditionVolumesReady says whether the room's volumes have the sizes its
// spec asks for, and why not.
const ConditionVolumesReady = "VolumesReady"

// VolumeClaimName is the PVC the StatefulSet made for a room's volume
// ("home" or "engine").
func VolumeClaimName(room, volume string) string { return volume + "-" + Name(room) + "-0" }

// resizeVolumes grows the room's home and engine claims to spec.volumes. A
// StatefulSet's claim templates can't change, so the claims are expanded
// directly, where their StorageClass allows it. Volumes never shrink.
func (r *Reconciler) resizeVolumes(ctx context.Context, room *v1.Room, st *v1.RoomStatus) error {
	want := map[string]resource.Quantity{"home": defaultHome, "engine": defaultEngine}
	if q := room.Spec.Volumes.Home; q != nil {
		want["home"] = *q
	}
	if q := room.Spec.Volumes.Engine; q != nil {
		want["engine"] = *q
	}
	var problems, pending []string
	for _, vol := range []string{"home", "engine"} {
		size := want[vol]
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Namespace: room.Namespace, Name: VolumeClaimName(room.Name, vol)}, pvc)
		if apierrors.IsNotFound(err) {
			continue // not created yet: the StatefulSet makes it at the spec's size
		}
		if err != nil {
			return err
		}
		cur := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		switch c := size.Cmp(cur); {
		case c < 0:
			problems = append(problems, fmt.Sprintf("%s stays at %s: volumes never shrink (asked for %s)", vol, cur.String(), size.String()))
			continue
		case c > 0:
			ok, why, err := r.expandable(ctx, pvc)
			if err != nil {
				return err
			}
			if !ok {
				problems = append(problems, fmt.Sprintf("%s stays at %s: %s", vol, cur.String(), why))
				continue
			}
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = size
			if err := r.Update(ctx, pvc); err != nil {
				problems = append(problems, fmt.Sprintf("%s: expanding to %s: %v", vol, size.String(), err))
				continue
			}
		}
		if pvc.Status.Phase != corev1.ClaimBound {
			continue // provisioned at its requested size once its pod is scheduled
		}
		if got, ok := pvc.Status.Capacity[corev1.ResourceStorage]; !ok || got.Cmp(size) < 0 {
			pending = append(pending, fmt.Sprintf("%s to %s", vol, size.String()))
		}
	}
	switch {
	case len(problems) > 0:
		setCond(st, room.Generation, ConditionVolumesReady, false, "NotResized", strings.Join(problems, "; "))
	case len(pending) > 0:
		setCond(st, room.Generation, ConditionVolumesReady, false, "Resizing", "expanding "+strings.Join(pending, ", "))
	default:
		setCond(st, room.Generation, ConditionVolumesReady, true, "Sized", "")
	}
	return nil
}

// expandable reports whether a claim's StorageClass allows expansion.
func (r *Reconciler) expandable(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (bool, string, error) {
	name := ""
	if pvc.Spec.StorageClassName != nil {
		name = *pvc.Spec.StorageClassName
	}
	if name == "" {
		return false, "the volume has no StorageClass, so it can't be expanded", nil
	}
	var sc storagev1.StorageClass
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "StorageClass " + name + " not found", nil
		}
		return false, "", err
	}
	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		return false, "StorageClass " + name + " doesn't allow volume expansion (allowVolumeExpansion is not true)", nil
	}
	return true, "", nil
}
