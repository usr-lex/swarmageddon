package room

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/egress"
	"github.com/usr-lex/swarmageddon/internal/operator/persona"
	"github.com/usr-lex/swarmageddon/internal/roomsettings"
	"github.com/usr-lex/swarmageddon/internal/warden/keytable"
)

// Conditions this controller adds besides the API's.
const (
	// ConditionSettingsReady is false when layered settings could not be
	// applied in full: a missing Secret, or a shared volume refused at a wall.
	ConditionSettingsReady = "SettingsReady"
)

// EgressProxyURL is the egress proxy rooms in limited mode use (§11.2).
var EgressProxyURL = fmt.Sprintf("http://egress.%s.svc:%d", v1.PlatformNamespace, egress.Port)

// NoProxy lists what rooms reach directly in limited mode: the platform
// services, cluster domains, and the workspace's allowedCIDRs.
func NoProxy(allowed []string) []string {
	out := []string{"localhost", "127.0.0.1", "::1", ".svc", ".svc.cluster.local", ".cluster.local",
		"messaging." + v1.PlatformNamespace + ".svc", "forgejo." + v1.PlatformNamespace + ".svc",
		"forgejo-ssh." + v1.PlatformNamespace + ".svc", "gateway." + v1.PlatformNamespace + ".svc",
		"registry-cache." + v1.PlatformNamespace + ".svc"}
	return append(out, allowed...)
}

// settings merges and resolves the room's layered settings (§6.6).
func (r *Reconciler) settings(ctx context.Context, room *v1.Room, ws *v1.Workspace, role *v1.RoleTemplate) (v1.RoomDefaults, persona.Settings, map[string]string, []error) {
	merged := persona.MergeFor(ws, role, room)
	cache := map[string]*corev1.Secret{}
	get := func(ref v1.SecretKeyRef) (string, error) {
		s, ok := cache[ref.Name]
		if !ok {
			s = &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: room.Namespace, Name: ref.Name}, s); err != nil {
				s = nil
			}
			cache[ref.Name] = s
		}
		if s == nil {
			return "", fmt.Errorf("Secret %s not found in %s", ref.Name, room.Namespace)
		}
		v, ok := s.Data[ref.Key]
		if !ok {
			return "", fmt.Errorf("Secret %s has no key %s", ref.Name, ref.Key)
		}
		return string(v), nil
	}
	set, values, errs := persona.Resolve(merged, get)
	set.Network = persona.NetworkFor(merged, EgressProxyURL, NoProxy(ws.Spec.AllowedCIDRs), egress.PackageManagers)
	for _, d := range set.Network.Domains {
		if !egress.ValidDomain(d) {
			errs = append(errs, fmt.Errorf("network domain %q is not a host, *.suffix or host:port", d))
		}
	}
	return merged, set, values, errs
}

// syncSecretEnv copies the secretEnv values into the identity Secret as
// env_<NAME>, removing variables no longer set.
func syncSecretEnv(sec *corev1.Secret, values map[string]string) {
	for k := range sec.Data {
		if strings.HasPrefix(k, "env_") {
			if _, ok := values[strings.TrimPrefix(k, "env_")]; !ok {
				delete(sec.Data, k)
			}
		}
	}
	for n, v := range values {
		sec.Data[persona.SecretEnvKey(n)] = []byte(v)
	}
}

// ---- tables the warden and the egress proxy read ----

// patchTable sets (or, with value nil, removes) one room's row of a table
// ConfigMap in the platform namespace. A merge patch of one data key never
// conflicts with other rooms' controllers.
func (r *Reconciler) patchTable(ctx context.Context, name, key string, value *string) error {
	cm := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Namespace: v1.PlatformNamespace, Name: name}, cm)
	if apierrors.IsNotFound(err) {
		if value == nil {
			return nil
		}
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: v1.PlatformNamespace,
				Labels: map[string]string{v1.LabelManaged: "true", v1.LabelComponent: name}},
			Data: map[string]string{key: *value},
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
	cur, ok := cm.Data[key]
	if (value == nil && !ok) || (value != nil && ok && cur == *value) {
		return nil
	}
	var v any
	if value != nil {
		v = *value
	}
	b, _ := json.Marshal(map[string]any{"data": map[string]any{key: v}})
	return r.Patch(ctx, cm, client.RawPatch(types.MergePatchType, b))
}

// syncTables writes the room's rows: its gateway keys for the warden, its
// allowlist for the egress proxy.
func (r *Reconciler) syncTables(ctx context.Context, room *v1.Room, ws *v1.Workspace, role *v1.RoleTemplate, sec *corev1.Secret, merged v1.RoomDefaults, set persona.Settings) error {
	keys := map[string]string{}
	for k, kind := range map[string]string{KeyGatewayKey: keytable.KindMain, KeyAdvisorKey: keytable.KindAdvisor, KeyWardenKey: keytable.KindWarden} {
		if v := string(sec.Data[k]); v != "" {
			keys[keytable.Hash(v)] = kind
		}
	}
	w := persona.Warden(room, role, ws)
	entry := keytable.Entry{
		Room: room.Name + "@" + ws.Name, Keys: keys, Quarantined: room.Spec.Quarantined,
		Mode: w.Mode, Model: w.Model, QuarantineOnHigh: w.QuarantineOnHigh, Rules: role.Spec.WardenRules,
	}
	row := entry.Marshal()
	if err := r.patchTable(ctx, keytable.ConfigMapName, keytable.DataKey(ws.Name, room.Name), &row); err != nil {
		return fmt.Errorf("warden key table: %w", err)
	}
	pm := merged.Network != nil && merged.Network.PackageManagers != nil && *merged.Network.PackageManagers
	e := egress.Entry{Room: entry.Room, Mode: set.Network.Mode, Auth: egress.Hash(string(sec.Data[KeyEgressAuth])),
		Domains: set.Network.Domains, PackageManagers: pm}
	if room.Spec.Quarantined {
		e.Mode, e.Domains = egress.ModeNone, nil
	}
	row = e.Marshal()
	if err := r.patchTable(ctx, egress.ConfigMapName, egress.DataKey(ws.Name, room.Name), &row); err != nil {
		return fmt.Errorf("egress allowlist: %w", err)
	}
	return nil
}

func (r *Reconciler) removeTables(ctx context.Context, ws, name string) error {
	if err := r.patchTable(ctx, keytable.ConfigMapName, keytable.DataKey(ws, name), nil); err != nil {
		return err
	}
	return r.patchTable(ctx, egress.ConfigMapName, egress.DataKey(ws, name), nil)
}

// ---- shared volumes (§6.6) ----

// SharedVolumeClaim is the PVC of a shared volume.
func SharedVolumeClaim(name string) string { return "shared-" + name }

// sharedMount is a shared volume mounted in a room.
type sharedMount struct {
	Name     string
	ReadOnly bool
}

func selects(sel string, r *v1.Room) bool {
	return roomsettings.Selects(sel, r.Name, r.Spec.Role, r.Spec.Group)
}

// WallViolations returns, by volume name, why a shared volume may not be
// mounted read-write as declared: rooms on both sides of a wall would write
// to it (a mediated or none link between their groups, roomsettings.Walled).
// rooms are the workspace's rooms. The room controller mounts such a
// volume read-only everywhere; the workspace controller can report it.
func WallViolations(ws *v1.Workspace, rooms []v1.Room) map[string]string {
	out := map[string]string{}
	for _, vol := range ws.Spec.SharedVolumes {
		groups := map[string]string{} // group -> a room in it
		for i := range rooms {
			rm := &rooms[i]
			for _, m := range vol.Mounts {
				if !m.ReadOnly && selects(m.Select, rm) {
					if _, ok := groups[rm.Spec.Group]; !ok {
						groups[rm.Spec.Group] = rm.Name
					}
				}
			}
		}
		gs := slices.Sorted(maps.Keys(groups))
	pairs:
		for i, a := range gs {
			for _, b := range gs[i+1:] {
				if roomsettings.Walled(ws.Spec.Messaging.Links, a, b) {
					out[vol.Name] = fmt.Sprintf("shared volume %s would be writable by %s (group %q) and %s (group %q), on both sides of a wall; it is mounted read-only",
						vol.Name, groups[a], a, groups[b], b)
					break pairs
				}
			}
		}
	}
	return out
}

// sharedVolumes ensures the claims of the volumes the room mounts and
// returns its mounts, and messages for volumes refused at a wall.
func (r *Reconciler) sharedVolumes(ctx context.Context, room *v1.Room, ws *v1.Workspace) ([]sharedMount, []string, error) {
	if len(ws.Spec.SharedVolumes) == 0 {
		return nil, nil, nil
	}
	var rooms v1.RoomList
	if err := r.List(ctx, &rooms, client.InNamespace(room.Namespace)); err != nil {
		return nil, nil, err
	}
	walls := WallViolations(ws, rooms.Items)
	var mounts []sharedMount
	var msgs []string
	for _, vol := range ws.Spec.SharedVolumes {
		matched, rw := false, false
		for _, m := range vol.Mounts {
			if selects(m.Select, room) {
				matched = true
				rw = rw || !m.ReadOnly
			}
		}
		if !matched {
			continue
		}
		if msg, ok := walls[vol.Name]; ok {
			msgs = append(msgs, msg)
			rw = false
		}
		if err := r.ensureSharedClaim(ctx, ws, room.Namespace, vol); err != nil {
			return nil, nil, err
		}
		mounts = append(mounts, sharedMount{Name: vol.Name, ReadOnly: !rw})
	}
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Name < mounts[j].Name })
	return mounts, msgs, nil
}

// ensureSharedClaim creates a shared volume's ReadWriteMany claim if it is
// missing. It never changes or deletes one: the data stays.
func (r *Reconciler) ensureSharedClaim(ctx context.Context, ws *v1.Workspace, ns string, vol v1.SharedVolume) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: SharedVolumeClaim(vol.Name)}, pvc)
	if err == nil || !apierrors.IsNotFound(err) {
		return err
	}
	size := vol.Size
	if size.IsZero() {
		size = resource.MustParse("10Gi")
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: SharedVolumeClaim(vol.Name), Namespace: ns, Labels: map[string]string{
			v1.LabelWorkspace: ws.Name, v1.LabelManaged: "true", v1.LabelComponent: "shared-volume",
		}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: vol.StorageClassName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if err := controllerutil.SetOwnerReference(ws, pvc, r.Scheme()); err != nil {
		return err
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
