package room

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/devices"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/messaging/revoked"
)

func TestUsernsPodsGetDevices(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", nil)
	e.reconcile("athena")
	c := e.sts("athena").Spec.Template.Spec.Containers[0]
	for _, rn := range []corev1.ResourceName{devices.ResourceFuse, devices.ResourceTun} {
		if q := c.Resources.Limits[rn]; q.Value() != 1 {
			t.Errorf("limit %s = %v", rn, q)
		}
	}
	// Other tiers don't use the device plugin.
	spec, ctr := corev1.PodSpec{}, corev1.Container{}
	ApplyTier(&spec, &ctr, string(v1.TierPrivileged))
	if _, ok := ctr.Resources.Limits[devices.ResourceFuse]; ok {
		t.Error("privileged tier requests the fuse device")
	}
}

func TestLeavesManagedTeamsOnly(t *testing.T) {
	e := newEnv(t)
	e.forge.Teams = append(e.forge.Teams, forgejo.Team{ID: 9, Name: "people"}) // made by hand in Forgejo
	e.addRoom("athena", func(r *v1.Room) { r.Spec.Group = "clean" })
	e.reconcile("athena")
	if len(e.forge.Calls("PUT", "/teams/4/members/athena")) == 0 {
		t.Fatal("not added to group-clean")
	}

	e.update("athena", func(r *v1.Room) { r.Spec.Group = "" })
	e.reconcile("athena")
	if len(e.forge.Calls("DELETE", "/teams/4/members/athena")) != 1 {
		t.Error("not removed from group-clean")
	}
	for _, kept := range []string{"/teams/1/members/athena", "/teams/3/members/athena", "/teams/9/members/athena"} {
		if n := len(e.forge.Calls("DELETE", kept)); n != 0 {
			t.Errorf("removed from %s", kept)
		}
	}
	// implementers (2) is managed and not the room's: removing is a no-op there.
	if len(e.forge.Calls("DELETE", "/teams/2/members/athena")) == 0 {
		t.Error("membership of other managed teams not checked")
	}
}

func TestGatewayKeysUpdatedInPlace(t *testing.T) {
	e := newEnv(t)
	e.role(func(r *v1.RoleTemplate) { r.Spec.Advisor = &v1.AdvisorSpec{Model: "big", MonthlyUSD: 30} })
	e.addRoom("athena", nil)
	e.reconcile("athena")
	keys := e.secret("athena")
	generated := len(e.gw.Calls("POST", "/key/generate"))

	e.workspace(func(w *v1.Workspace) { w.Spec.Models.Budgets.PerRoomMonthlyUSD = 300 })
	e.update("athena", func(r *v1.Room) { r.Spec.Model = "claude-sonnet" })
	e.role(func(r *v1.RoleTemplate) { r.Spec.Advisor = &v1.AdvisorSpec{Model: "bigger", MonthlyUSD: 40} })
	e.reconcile("athena")

	if n := len(e.gw.Calls("POST", "/key/generate")); n != generated {
		t.Errorf("keys re-issued (%d -> %d generate calls)", generated, n)
	}
	sec := e.secret("athena")
	for _, k := range []string{KeyGatewayKey, KeyAdvisorKey} {
		if string(sec.Data[k]) != string(keys.Data[k]) {
			t.Errorf("%s changed", k)
		}
	}
	var main, advisor map[string]any
	for _, c := range e.gw.Calls("POST", "/key/update") {
		switch c.Body["key"] {
		case string(keys.Data[KeyGatewayKey]):
			main = c.Body
		case string(keys.Data[KeyAdvisorKey]):
			advisor = c.Body
		}
	}
	if main == nil || main["max_budget"] != 300.0 || main["metadata"].(map[string]any)["model"] != "claude-sonnet" {
		t.Errorf("main key update = %v", main)
	}
	if advisor == nil || advisor["max_budget"] != 40.0 || advisor["models"].([]any)[0] != "bigger" {
		t.Errorf("advisor key update = %v", advisor)
	}
	// Stable afterwards.
	n := len(e.gw.Calls("POST", "/key/update"))
	e.reconcile("athena")
	if len(e.gw.Calls("POST", "/key/update")) != n {
		t.Error("keys updated again on a no-op reconcile")
	}
}

func TestVolumesGrowNeverShrink(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must(t, e.c.Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "grows"}, Provisioner: "example.com/x", AllowVolumeExpansion: ptr.To(true)}))
	must(t, e.c.Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fixed"}, Provisioner: "example.com/x"}))
	e.addRoom("athena", func(r *v1.Room) {
		r.Spec.Volumes = v1.RoomVolumes{Home: ptr.To(resource.MustParse("10Gi")), Engine: ptr.To(resource.MustParse("20Gi"))}
	})
	e.reconcile("athena")
	// envtest has no StatefulSet controller: make the bound claims it would.
	claim := func(vol, class, size string) {
		p := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: VolumeClaimName("athena", vol), Namespace: ns, Labels: SelectorLabels("athena")},
			Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: ptr.To(class), VolumeName: "pv-" + vol,
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}},
		}
		must(t, e.c.Create(ctx, p))
		p.Status = corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, AccessModes: p.Spec.AccessModes,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}
		must(t, e.c.Status().Update(ctx, p))
	}
	claim("home", "grows", "10Gi")
	claim("engine", "fixed", "20Gi")
	request := func(vol string) string {
		var p corev1.PersistentVolumeClaim
		must(t, e.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: VolumeClaimName("athena", vol)}, &p))
		q := p.Spec.Resources.Requests[corev1.ResourceStorage]
		return q.String()
	}
	cond := func() *metav1.Condition {
		return meta.FindStatusCondition(e.room("athena").Status.Conditions, ConditionVolumesReady)
	}
	e.reconcile("athena")
	if c := cond(); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v", c)
	}
	before := e.snap("athena")

	e.update("athena", func(r *v1.Room) {
		r.Spec.Volumes.Home = ptr.To(resource.MustParse("40Gi"))
		r.Spec.Volumes.Engine = ptr.To(resource.MustParse("50Gi"))
	})
	e.reconcile("athena")
	if got := request("home"); got != "40Gi" {
		t.Errorf("home request = %s", got)
	}
	if got := request("engine"); got != "20Gi" {
		t.Errorf("engine request = %s (its class doesn't expand)", got)
	}
	c := cond()
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "fixed doesn't allow volume expansion") {
		t.Errorf("condition = %+v", c)
	}
	if got := e.snap("athena"); got.hash != before.hash || got.uid != before.uid {
		t.Error("resizing restarted the room")
	}

	e.update("athena", func(r *v1.Room) {
		r.Spec.Volumes.Home = ptr.To(resource.MustParse("5Gi"))
		r.Spec.Volumes.Engine = ptr.To(resource.MustParse("20Gi"))
	})
	e.reconcile("athena")
	if got := request("home"); got != "40Gi" {
		t.Errorf("home shrank to %s", got)
	}
	if c := cond(); c == nil || !strings.Contains(c.Message, "never shrink") {
		t.Errorf("condition = %+v", c)
	}
}

func TestRemovedRoomsCertificateIsRevoked(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", nil)
	e.reconcile("athena")
	key, _, err := revoked.Entry(e.secret("athena").Data[KeyTLSCert])
	must(t, err)

	// Rotation revokes the old certificate.
	e.update("athena", func(r *v1.Room) { r.Annotations = map[string]string{v1.AnnotationRotate: "1"} })
	e.reconcile("athena")
	if _, ok := e.table(revoked.ConfigMapName)[key]; !ok {
		t.Fatalf("rotated certificate %s not on the deny list: %v", key, e.table(revoked.ConfigMapName))
	}
	key2, _, err := revoked.Entry(e.secret("athena").Data[KeyTLSCert])
	must(t, err)

	// An expired entry is dropped on the next write.
	must(t, e.r.patchTable(context.Background(), revoked.ConfigMapName, "dead", ptr.To("2020-01-01T00:00:00Z old@acme")))

	must(t, e.c.Delete(context.Background(), e.room("athena")))
	e.reconcile("athena")
	deny := e.table(revoked.ConfigMapName)
	if _, ok := deny[key2]; !ok || !strings.HasSuffix(deny[key2], " athena@acme") {
		t.Errorf("deny list = %v, want %s", deny, key2)
	}
	if _, ok := deny["dead"]; ok {
		t.Error("expired entry kept")
	}
}
