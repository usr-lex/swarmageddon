// Package room is the Room controller (docs/design.md §5): for one Room it
// reconciles exactly that room's objects (ServiceAccount, identity Secret,
// persona ConfigMap, StatefulSet, headless Service, NetworkPolicy), its
// Forgejo account and its gateway key. One room never restarts another.
package room

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/persona"
)

const (
	// Finalizer disables the forge user and deletes the gateway key.
	Finalizer = "swarmageddon.dev/room"
	// AnnotationDeleteDataNow on a Room being deleted removes its volumes at once.
	AnnotationDeleteDataNow = "swarmageddon.dev/delete-data-now"
	// AnnotationDeleteAfter on a PVC of a removed room: RFC 3339 time it is deleted at.
	AnnotationDeleteAfter = "swarmageddon.dev/delete-after"

	annRotateHandled   = "swarmageddon.dev/rotate-handled"
	annRecreateHandled = "swarmageddon.dev/recreate-handled"
	annUpdateHandled   = "swarmageddon.dev/update-now-handled"
	annForgeSynced     = "swarmageddon.dev/forge-synced"
	annForgeTokenName  = "swarmageddon.dev/forge-token-name"
	annGatewayAlias    = "swarmageddon.dev/gateway-key-alias"
	annGatewayKeySpec  = "swarmageddon.dev/gateway-key-spec"
	annForgePaused     = "swarmageddon.dev/forge-paused"
	annRoomSpecHash    = "swarmageddon.dev/room-spec-hash"

	certTTL                 = 30 * 24 * time.Hour
	defaultDeletedRetention = 7 * 24 * time.Hour
	forgeTokenPrefix        = "swarm-"
)

// IdleChecker tells whether a room is idle (presence idle, no turn, no
// pending approvals), so it may be rolled to a new image. The lead wires it
// to messaging.
type IdleChecker interface {
	Idle(ctx context.Context, workspace, room string) (bool, error)
}

// NeverIdle is the default IdleChecker: rooms update only on "update now".
type NeverIdle struct{}

// Idle always reports false.
func (NeverIdle) Idle(context.Context, string, string) (bool, error) { return false, nil }

// Reconciler reconciles Rooms.
type Reconciler struct {
	client.Client
	Deps deps.Deps
	Idle IdleChecker
	// ScanHostKey fetches Forgejo's SSH host key; nil skips it.
	ScanHostKey HostKeyScanner
	Now         func() time.Time

	mu       sync.Mutex
	lastRoll map[string]time.Time // workspace -> when a room was last rolled
}

// +kubebuilder:rbac:groups=swarmageddon.dev,resources=rooms;rooms/status;rooms/finalizers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmageddon.dev,resources=roletemplates;workspaces;platforms,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;secrets;configmaps;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch

// SetupWithManager registers the Room controller.
func SetupWithManager(mgr ctrl.Manager, d deps.Deps) error {
	r := &Reconciler{Client: mgr.GetClient(), Deps: d, Idle: NeverIdle{}, ScanHostKey: ScanHostKey}
	return r.SetupWithManager(mgr)
}

// SetupWithManager registers r.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	roomsInNamespace := func(ctx context.Context, ns string, match func(*v1.Room) bool) []reconcile.Request {
		var rooms v1.RoomList
		if err := r.List(ctx, &rooms, client.InNamespace(ns)); err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for i := range rooms.Items {
			if match(&rooms.Items[i]) {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&rooms.Items[i])})
			}
		}
		return reqs
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("room").
		For(&v1.Room{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&v1.RoleTemplate{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			return roomsInNamespace(ctx, o.GetNamespace(), func(rm *v1.Room) bool { return rm.Spec.Role == o.GetName() })
		})).
		Watches(&v1.Workspace{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			return roomsInNamespace(ctx, v1.WorkspaceNamespace(o.GetName()), func(*v1.Room) bool { return true })
		})).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			room := o.GetLabels()[v1.LabelRoom]
			if room == "" || o.GetLabels()[v1.LabelComponent] != ComponentRoom {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: room}}}
		})).
		Complete(r)
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Reconcile brings one room's objects to their desired state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var room v1.Room
	if err := r.Get(ctx, req.NamespacedName, &room); err != nil {
		if apierrors.IsNotFound(err) {
			return r.cleanupVolumes(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}
	ws := WorkspaceOf(room.Namespace)
	if !room.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &room, ws)
	}
	if controllerutil.AddFinalizer(&room, Finalizer) {
		if err := r.Update(ctx, &room); err != nil {
			return ctrl.Result{}, err
		}
	}
	st := room.Status.DeepCopy()
	res, err := r.reconcile(ctx, &room, ws, st)
	st.ObservedGeneration = room.Generation
	if err != nil {
		log.FromContext(ctx).Error(err, "reconcile room")
	}
	if !equalStatus(&room.Status, st) {
		room.Status = *st
		if uerr := r.Status().Update(ctx, &room); uerr != nil && err == nil {
			err = uerr
		}
	}
	return res, err
}

func equalStatus(a, b *v1.RoomStatus) bool {
	strip := func(s *v1.RoomStatus) v1.RoomStatus {
		c := *s.DeepCopy()
		for i := range c.Conditions {
			c.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		return c
	}
	x, y := strip(a), strip(b)
	return fmt.Sprint(x) == fmt.Sprint(y)
}

func setCond(st *v1.RoomStatus, gen int64, typ string, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen})
}

func (r *Reconciler) reconcile(ctx context.Context, room *v1.Room, wsName string, st *v1.RoomStatus) (ctrl.Result, error) {
	gen := room.Generation
	retry := ctrl.Result{RequeueAfter: 30 * time.Second}
	st.Address = room.Name + "@" + wsName

	var ws v1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: wsName}, &ws); err != nil {
		st.Phase = v1.RoomProvisioning
		setCond(st, gen, v1.ConditionPodReady, false, "WorkspaceMissing", fmt.Sprintf("workspace %q: %v", wsName, err))
		return retry, client.IgnoreNotFound(err)
	}
	var role v1.RoleTemplate
	if err := r.Get(ctx, types.NamespacedName{Namespace: room.Namespace, Name: room.Spec.Role}, &role); err != nil {
		st.Phase = v1.RoomProvisioning
		setCond(st, gen, v1.ConditionPodReady, false, "RoleTemplateMissing", fmt.Sprintf("role template %q: %v", room.Spec.Role, err))
		return retry, client.IgnoreNotFound(err)
	}
	r.keepVolumes(ctx, room)
	if err := r.noteQuarantine(ctx, room, st); err != nil {
		return retry, err
	}

	merged, set, secretValues, problems := r.settings(ctx, room, &ws, &role)
	shared, wallMsgs, err := r.sharedVolumes(ctx, room, &ws)
	if err != nil {
		return retry, err
	}
	var msgs []string
	for _, e := range problems {
		msgs = append(msgs, e.Error())
	}
	msgs = append(msgs, wallMsgs...)
	if len(msgs) > 0 {
		setCond(st, gen, ConditionSettingsReady, false, "Incomplete", strings.Join(msgs, "; "))
	} else {
		setCond(st, gen, ConditionSettingsReady, true, "Applied", "network "+set.Network.Mode)
	}

	if err := r.apply(ctx, room, desiredServiceAccount(room, wsName)); err != nil {
		return retry, err
	}
	sec, ready, err := r.reconcileIdentity(ctx, room, &ws, &role, st, secretValues)
	if err != nil {
		return retry, err
	}

	data, err := persona.Render(persona.Input{Room: room, Role: &role, Workspace: &ws, Settings: &set, Endpoints: persona.Endpoints{
		ForgeURL: ForgeURL, ForgeSSHHost: ForgeSSHHost, MessagingURL: MessagingURL, GatewayURL: GatewayURL,
	}})
	if err != nil {
		return retry, err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: PersonaConfigMapName(room.Name), Namespace: room.Namespace, Labels: objectLabels(room, wsName)}, Data: data}
	if err := r.apply(ctx, room, cm); err != nil {
		return retry, err
	}
	if err := r.apply(ctx, room, desiredService(room, wsName)); err != nil {
		return retry, err
	}
	np := desiredNetworkPolicy(room, wsName, r.egress(ctx, &ws, persona.NetworkMode(merged), room.Spec.Quarantined))
	if err := r.apply(ctx, room, np); err != nil {
		return retry, err
	}
	if err := r.syncTables(ctx, room, &ws, &role, sec, merged, set); err != nil {
		return retry, err
	}

	res, err := r.reconcileStatefulSet(ctx, room, &ws, st, podExtras{Env: set.Env, Shared: shared, PersonaHash: dataHash(data)})
	if err != nil {
		return retry, err
	}
	if err := r.resizeVolumes(ctx, room, st); err != nil {
		return retry, err
	}
	if !ready && res.RequeueAfter == 0 {
		res = retry
	}
	if res.RequeueAfter == 0 {
		res.RequeueAfter = 12 * time.Hour // certificate renewal
	}
	return res, nil
}

// apply creates obj or updates its labels and content, owned by room.
func (r *Reconciler) apply(ctx context.Context, room *v1.Room, obj client.Object) error {
	if err := controllerutil.SetControllerReference(room, obj, r.Scheme()); err != nil {
		return err
	}
	cur := obj.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), cur)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	changed := false
	if !maps.Equal(cur.GetLabels(), obj.GetLabels()) {
		cur.SetLabels(obj.GetLabels())
		changed = true
	}
	switch want := obj.(type) {
	case *corev1.ConfigMap:
		c := cur.(*corev1.ConfigMap)
		if !maps.Equal(c.Data, want.Data) {
			c.Data, changed = want.Data, true
		}
	case *corev1.Service:
		c := cur.(*corev1.Service)
		if fmt.Sprint(c.Spec.Ports, c.Spec.Selector) != fmt.Sprint(want.Spec.Ports, want.Spec.Selector) {
			c.Spec.Ports, c.Spec.Selector, changed = want.Spec.Ports, want.Spec.Selector, true
		}
	case *networkingv1.NetworkPolicy:
		c := cur.(*networkingv1.NetworkPolicy)
		if !specEqual(c.Spec, want.Spec) {
			c.Spec, changed = want.Spec, true
		}
	case *corev1.ServiceAccount:
		c := cur.(*corev1.ServiceAccount)
		if c.AutomountServiceAccountToken == nil || *c.AutomountServiceAccountToken {
			c.AutomountServiceAccountToken, changed = want.AutomountServiceAccountToken, true
		}
	}
	if !changed {
		return nil
	}
	return r.Update(ctx, cur)
}

func specEqual(a, b networkingv1.NetworkPolicySpec) bool {
	return a.String() == b.String()
}

// dataHash is a stable hash of ConfigMap data.
func dataHash(data map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(data)) {
		h.Write([]byte(k + "\x00" + data[k] + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// egress is a room's egress for its network mode on this platform (§11.2).
func (r *Reconciler) egress(ctx context.Context, ws *v1.Workspace, mode v1.NetworkMode, quarantined bool) Egress {
	e := Egress{Internet: mode == v1.NetworkFull, Proxy: mode == v1.NetworkLimited, Quarantined: quarantined}
	if mode != v1.NetworkNone {
		e.Allowed = ws.Spec.AllowedCIDRs
	}
	var p v1.Platform
	if err := r.Get(ctx, types.NamespacedName{Name: v1.PlatformName}, &p); err == nil {
		cl := p.Spec.Egress.ClusterCIDRs
		if len(cl) == 0 && p.Status.Preflight != nil {
			cl = p.Status.Preflight.ClusterCIDRs
		}
		e.Denied = append(append(e.Denied, cl...), p.Spec.Egress.ExtraDeniedCIDRs...)
	}
	return e
}

// ---- identity, forge account, gateway key ----

func (r *Reconciler) reconcileIdentity(ctx context.Context, room *v1.Room, ws *v1.Workspace, role *v1.RoleTemplate, st *v1.RoomStatus, secretEnv map[string]string) (*corev1.Secret, bool, error) {
	gen := room.Generation
	name, wsName := room.Name, ws.Name
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: room.Namespace, Name: IdentitySecretName(name)}, sec)
	isNew := apierrors.IsNotFound(err)
	if err != nil && !isNew {
		return nil, false, err
	}
	if isNew {
		sec = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: IdentitySecretName(name), Namespace: room.Namespace}}
	}
	orig := sec.DeepCopy()
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	sec.Labels = objectLabels(room, wsName)
	sec.Type = corev1.SecretTypeOpaque

	rotate := room.Annotations[v1.AnnotationRotate]
	rotating := rotate != "" && rotate != sec.Annotations[annRotateHandled]
	email := persona.Email(name, wsName)

	// Local keys.
	if rotating || len(sec.Data[KeySSHPrivate]) == 0 {
		priv, pub, err := newSSHKey(email)
		if err != nil {
			return nil, false, err
		}
		sec.Data[KeySSHPrivate], sec.Data[KeySSHPublic] = priv, pub
	}
	if rotating || len(sec.Data[KeyGPGSecret]) == 0 {
		kp, err := forgejo.NewGPGKey(displayName(room), email)
		if err != nil {
			return nil, false, err
		}
		sec.Data[KeyGPGSecret] = []byte(kp.ArmoredPrivate)
	}
	if rotating || len(sec.Data[KeyVNCPassword]) == 0 {
		sec.Data[KeyVNCPassword] = []byte(randomString(6)) // VNC uses at most 8 characters
	}
	if rotating || len(sec.Data[KeyEgressAuth]) == 0 {
		sec.Data[KeyEgressAuth] = []byte(randomString(24))
	}
	syncSecretEnv(sec, secretEnv)
	setCond(st, gen, v1.ConditionKeysReady, true, "Generated", "SSH, GPG and VNC credentials are in "+sec.Name)

	// Messaging identity. A rotated certificate goes on messaging's deny
	// list once the Secret holds the new one.
	var revokeLater []func() error
	if r.Deps.PKI == nil {
		setCond(st, gen, v1.ConditionMessagingIdentityReady, false, "NoCA", "the internal CA is not available yet")
	} else if rotating || certNeedsRenewal(sec.Data[KeyTLSCert], r.now()) {
		uri := fmt.Sprintf("spiffe://swarmageddon/ws/%s/room/%s", wsName, name)
		crt, key, err := r.Deps.PKI.IssueClientCert(name+"@"+wsName, uri, certTTL)
		if err != nil {
			setCond(st, gen, v1.ConditionMessagingIdentityReady, false, "IssueFailed", err.Error())
		} else {
			if old := sec.Data[KeyTLSCert]; rotating && len(old) > 0 {
				revokeLater = append(revokeLater, func() error { return r.revokeCert(ctx, old) })
			}
			sec.Data[KeyTLSCert], sec.Data[KeyTLSKey] = crt, key
			setCond(st, gen, v1.ConditionMessagingIdentityReady, true, "Issued", "CN="+name+"@"+wsName)
		}
	} else {
		setCond(st, gen, v1.ConditionMessagingIdentityReady, true, "Issued", "CN="+name+"@"+wsName)
	}
	if r.Deps.PKI != nil {
		sec.Data[KeyCA] = r.Deps.PKI.CABundle()
	}

	if len(sec.Data[KeyForgeKnownHost]) == 0 && r.ScanHostKey != nil {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if kh, err := r.ScanHostKey(sctx, ForgeSSHHost, 22); err == nil {
			sec.Data[KeyForgeKnownHost] = kh
		}
		cancel()
	}
	if _, ok := sec.Data[KeyForgeKnownHost]; !ok {
		sec.Data[KeyForgeKnownHost] = []byte{} // filled once Forgejo's SSH answers
	}

	// Remote accounts. Revocations of old credentials run after the Secret
	// holds the new ones.
	revoke := revokeLater
	var forgeOK bool
	if room.Spec.Quarantined {
		forgeOK = r.pauseForge(ctx, room, sec, st)
	} else {
		forgeOK = r.syncForge(ctx, room, ws, role, sec, rotating, st, &revoke)
		if forgeOK {
			delete(sec.Annotations, annForgePaused)
		}
	}
	gwOK := r.syncGatewayKey(ctx, room, ws, sec, rotating, st, &revoke)
	if gwOK {
		gwOK = r.syncPurposeKeys(ctx, room, ws, role, sec, rotating, st, &revoke)
	}
	if rotating {
		sec.Annotations[annRotateHandled] = rotate
		st.CredentialsRotatedAt = &metav1.Time{Time: r.now()}
	}

	if isNew {
		if err := controllerutil.SetControllerReference(room, sec, r.Scheme()); err != nil {
			return nil, false, err
		}
		if err := r.Create(ctx, sec); err != nil {
			return nil, false, err
		}
	} else if !maps.EqualFunc(orig.Data, sec.Data, bytes.Equal) || !maps.Equal(orig.Annotations, sec.Annotations) || !maps.Equal(orig.Labels, sec.Labels) {
		if err := r.Update(ctx, sec); err != nil {
			return nil, false, err
		}
	}
	for _, f := range revoke {
		if err := f(); err != nil {
			log.FromContext(ctx).Error(err, "revoke old credential")
		}
	}
	return sec, forgeOK && gwOK, nil
}

func displayName(room *v1.Room) string {
	if room.Spec.DisplayName != "" {
		return room.Spec.DisplayName
	}
	return room.Name
}

// forgeTeams are the teams the room's user joins: its role's, plus workspace
// teams that list it.
func forgeTeams(room *v1.Room, ws *v1.Workspace, role *v1.RoleTemplate) []string {
	set := map[string]bool{}
	for _, t := range role.Spec.ForgeTeams {
		set[t] = true
	}
	for _, t := range ws.Spec.Forge.Teams {
		if slices.Contains(t.Members, room.Name) {
			set[t.Name] = true
		}
	}
	if room.Spec.Group != "" {
		set["group-"+room.Spec.Group] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// BuiltinTeams are the teams every workspace's org has (design §9.1).
var BuiltinTeams = []string{"implementers", "reviewers", "mergers", "ci", "observers"}

// IsManagedTeam reports whether the operator manages the team: a built-in
// team, a group's team (group-*) or one in Workspace.spec.forge.teams.
func IsManagedTeam(ws *v1.Workspace, name string) bool {
	if slices.Contains(BuiltinTeams, name) || strings.HasPrefix(name, "group-") {
		return true
	}
	return slices.ContainsFunc(ws.Spec.Forge.Teams, func(t v1.TeamSpec) bool { return t.Name == name })
}

func (r *Reconciler) syncForge(ctx context.Context, room *v1.Room, ws *v1.Workspace, role *v1.RoleTemplate, sec *corev1.Secret, rotating bool, st *v1.RoomStatus, revoke *[]func() error) bool {
	gen := room.Generation
	fail := func(reason string, err error) bool {
		setCond(st, gen, v1.ConditionForgeAccountReady, false, reason, err.Error())
		return false
	}
	if r.Deps.Forge == nil {
		return fail("ForgeUnavailable", errors.New("no forge client"))
	}
	kp, _, err := forgejo.ParseGPGKey(string(sec.Data[KeyGPGSecret]))
	if err != nil {
		return fail("BadGPGKey", err)
	}
	teams := forgeTeams(room, ws, role)
	h := sha256.Sum256([]byte(strings.Join([]string{
		string(sec.Data[KeySSHPublic]), kp.KeyID, strings.Join(teams, ","), sec.Annotations[annForgeTokenName], displayName(room),
	}, "\n")))
	syncKey := hex.EncodeToString(h[:8])
	if !rotating && len(sec.Data[KeyForgeToken]) > 0 && sec.Annotations[annForgeSynced] == syncKey {
		setCond(st, gen, v1.ConditionForgeAccountReady, true, "Provisioned", "forge user "+room.Name)
		st.ForgeUser = room.Name
		return true
	}
	fc, err := r.Deps.Forge(ctx)
	if err != nil {
		return fail("ForgeUnavailable", err)
	}
	name, email := room.Name, persona.Email(room.Name, ws.Name)
	if _, err := fc.EnsureAgentUser(ctx, name, email, displayName(room), "x"+randomString(24)+"!A1"); err != nil {
		return fail("UserFailed", err)
	}
	st.ForgeUser = name
	if err := fc.EnsureUserKey(ctx, name, "swarm", string(sec.Data[KeySSHPublic])); err != nil {
		return fail("SSHKeyFailed", err)
	}
	priv := string(sec.Data[KeyGPGSecret])
	if err := fc.EnsureGPGKey(ctx, name, kp.KeyID, kp.ArmoredPublic, func(tok string) (string, error) {
		return forgejo.SignToken(priv, tok)
	}); err != nil {
		return fail("GPGKeyFailed", err)
	}
	if rotating || len(sec.Data[KeyForgeToken]) == 0 {
		tokName := fmt.Sprintf("%s%d", forgeTokenPrefix, r.now().UnixNano())
		t, err := fc.CreateUserToken(ctx, name, tokName, forgejo.AgentTokenScopes)
		if err != nil {
			return fail("TokenFailed", err)
		}
		sec.Data[KeyForgeToken] = []byte(t.SHA1)
		sec.Annotations[annForgeTokenName] = tokName
		*revoke = append(*revoke, func() error { return fc.DeleteTokensExcept(ctx, name, forgeTokenPrefix, tokName) })
	}
	orgTeams, err := fc.ListTeams(ctx, ws.Spec.Forge.Org)
	if err != nil {
		return fail("TeamsFailed", err)
	}
	var missing []string
	for _, tn := range teams {
		i := slices.IndexFunc(orgTeams, func(t forgejo.Team) bool { return t.Name == tn })
		if i < 0 {
			missing = append(missing, tn)
			continue
		}
		if err := fc.AddTeamMember(ctx, orgTeams[i].ID, name); err != nil {
			return fail("TeamsFailed", err)
		}
	}
	// Leave the managed teams the room no longer belongs to; teams people
	// made by hand in Forgejo are theirs to manage.
	for _, t := range orgTeams {
		if slices.Contains(teams, t.Name) || !IsManagedTeam(ws, t.Name) {
			continue
		}
		if err := fc.RemoveTeamMember(ctx, t.ID, name); err != nil {
			return fail("TeamsFailed", err)
		}
	}
	if len(missing) > 0 {
		return fail("TeamsMissing", fmt.Errorf("teams not in org %s yet: %s", ws.Spec.Forge.Org, strings.Join(missing, ", ")))
	}
	// Recompute: the token name is part of the key.
	h = sha256.Sum256([]byte(strings.Join([]string{
		string(sec.Data[KeySSHPublic]), kp.KeyID, strings.Join(teams, ","), sec.Annotations[annForgeTokenName], displayName(room),
	}, "\n")))
	sec.Annotations[annForgeSynced] = hex.EncodeToString(h[:8])
	setCond(st, gen, v1.ConditionForgeAccountReady, true, "Provisioned", "forge user "+name)
	return true
}

// roomModel is the model a room runs: its own, else the workspace default.
func roomModel(room *v1.Room, ws *v1.Workspace) string {
	if room.Spec.Model != "" {
		return room.Spec.Model
	}
	return ws.Spec.Models.Default
}

func (r *Reconciler) syncGatewayKey(ctx context.Context, room *v1.Room, ws *v1.Workspace, sec *corev1.Secret, rotating bool, st *v1.RoomStatus, revoke *[]func() error) bool {
	gen := room.Generation
	budget := ws.Spec.Models.Budgets.PerRoomMonthlyUSD
	want := fmt.Sprintf("%d/%s", budget, roomModel(room, ws))
	old := string(sec.Data[KeyGatewayKey])
	if !rotating && old != "" && sec.Annotations[annGatewayKeySpec] == want {
		setCond(st, gen, v1.ConditionGatewayKeyReady, true, "Issued", "alias "+sec.Annotations[annGatewayAlias])
		return true
	}
	if r.Deps.Gateway == nil {
		setCond(st, gen, v1.ConditionGatewayKeyReady, false, "GatewayUnavailable", "no gateway client")
		return false
	}
	gc, err := r.Deps.Gateway(ctx)
	if err != nil {
		setCond(st, gen, v1.ConditionGatewayKeyReady, false, "GatewayUnavailable", err.Error())
		return false
	}
	withModel := func(u gateway.KeyUpdate) gateway.KeyUpdate {
		u.Metadata["model"] = roomModel(room, ws)
		return u
	}
	// A new budget or model updates the key in place; the room keeps it.
	if !rotating && old != "" {
		err := gc.UpdateKey(ctx, old, withModel(gateway.RoomKeyUpdate(ws.Name, room.Name, budget, nil)))
		if err == nil {
			sec.Annotations[annGatewayKeySpec] = want
			setCond(st, gen, v1.ConditionGatewayKeyReady, true, "Updated", "alias "+sec.Annotations[annGatewayAlias])
			return true
		}
		if !gateway.IsNotFound(err) {
			setCond(st, gen, v1.ConditionGatewayKeyReady, false, "KeyUpdateFailed", err.Error())
			return false
		}
		// The key is gone from the gateway: issue a new one.
	}
	alias := fmt.Sprintf("room-%s-%s-%d", ws.Name, room.Name, r.now().Unix())
	req := gateway.RoomKeyRequest(ws.Name, room.Name, alias, budget, nil)
	req.Metadata["model"] = roomModel(room, ws)
	k, err := gc.GenerateKey(ctx, req)
	if err != nil {
		setCond(st, gen, v1.ConditionGatewayKeyReady, false, "KeyFailed", err.Error())
		return false
	}
	if old != "" {
		*revoke = append(*revoke, func() error { return gc.DeleteKeys(ctx, old) })
	}
	sec.Data[KeyGatewayKey] = []byte(k.Key)
	sec.Annotations[annGatewayAlias] = alias
	sec.Annotations[annGatewayKeySpec] = want
	setCond(st, gen, v1.ConditionGatewayKeyReady, true, "Issued", "alias "+alias)
	return true
}

// ---- StatefulSet, image updates, recreate ----

func (r *Reconciler) tier(ctx context.Context) string {
	if r.Deps.PlatformTier == nil {
		return string(v1.TierUserns)
	}
	t, err := r.Deps.PlatformTier(ctx)
	if err != nil || t == "" || t == string(v1.TierAuto) {
		return string(v1.TierUserns)
	}
	return t
}

func (r *Reconciler) roomImage() string {
	if r.Deps.Images == nil {
		return ""
	}
	return r.Deps.Images.Room()
}

// roomSpecHash is a hash of the room's own spec, leaving out what doesn't
// shape its pod (quarantine).
func roomSpecHash(room *v1.Room) string {
	spec := room.Spec.DeepCopy()
	spec.Quarantined = false
	b, _ := json.Marshal(spec)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// reconcileStatefulSet renders the room's StatefulSet. A change of the room's
// own spec applies at once. Everything else that would restart the pod (a
// new image, workspace or role defaults, the persona) waits for "update now"
// or the workspace's update policy, which rolls one idle room at a time.
func (r *Reconciler) reconcileStatefulSet(ctx context.Context, room *v1.Room, ws *v1.Workspace, st *v1.RoomStatus, x podExtras) (ctrl.Result, error) {
	gen := room.Generation
	target := room.Spec.Image
	if target == "" {
		target = r.roomImage()
	}
	tier := r.tier(ctx)
	specHash := roomSpecHash(room)
	full := desiredStatefulSet(room, ws.Name, target, tier, x)
	full.Annotations[annRoomSpecHash] = specHash

	cur := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: room.Namespace, Name: Name(room.Name)}, cur)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(room, full, r.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, full); err != nil {
			return ctrl.Result{}, err
		}
		st.Phase = v1.RoomProvisioning
		st.UpdateAvailable, st.ImageVersion = false, target
		setCond(st, gen, v1.ConditionPodReady, false, "Creating", "StatefulSet created")
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	var res ctrl.Result
	ann := map[string]string{}
	maps.Copy(ann, cur.Annotations)
	if ann[annRoomSpecHash] == "" {
		ann[annRoomSpecHash] = specHash // adopt a StatefulSet from before this annotation
	}
	roomChanged := ann[annRoomSpecHash] != specHash
	inUse := ""
	if cs := cur.Spec.Template.Spec.Containers; len(cs) > 0 {
		inUse = cs[0].Image
	}
	want, image := full, target
	if cur.Annotations[AnnotationTemplateHash] != full.Annotations[AnnotationTemplateHash] && room.Spec.Image == "" && !room.Spec.Paused {
		now := room.Annotations[v1.AnnotationUpdateNow]
		switch {
		case now != "" && now != ann[annUpdateHandled]:
			ann[annUpdateHandled] = now
			r.noteRoll(ws.Name)
		case r.mayRollAutomatically(ctx, room, ws):
			r.noteRoll(ws.Name)
		case roomChanged:
			// The room's own change applies now; a new image still waits.
			if inUse != "" && inUse != target {
				want, image = desiredStatefulSet(room, ws.Name, inUse, tier, x), inUse
			}
			res.RequeueAfter = time.Minute
		default:
			want, image = nil, inUse
			res.RequeueAfter = time.Minute
		}
	}

	recreate := room.Annotations[v1.AnnotationRecreate]
	if recreate != "" && recreate != ann[annRecreateHandled] {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: room.Namespace, Name: Name(room.Name) + "-0"}}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return res, err
		}
		ann[annRecreateHandled] = recreate
	}

	changed := false
	if want != nil {
		ann[AnnotationTemplateHash] = want.Annotations[AnnotationTemplateHash]
		ann[annRoomSpecHash] = specHash
		if cur.Annotations[AnnotationTemplateHash] != ann[AnnotationTemplateHash] {
			cur.Spec.Template = want.Spec.Template
			cur.Spec.UpdateStrategy = want.Spec.UpdateStrategy
			cur.Spec.PersistentVolumeClaimRetentionPolicy = want.Spec.PersistentVolumeClaimRetentionPolicy
			changed = true
		}
		st.UpdateAvailable = ann[AnnotationTemplateHash] != full.Annotations[AnnotationTemplateHash]
	} else {
		st.UpdateAvailable = true
	}
	st.ImageVersion = image
	if cur.Spec.Replicas == nil || *cur.Spec.Replicas != *full.Spec.Replicas {
		cur.Spec.Replicas = full.Spec.Replicas
		changed = true
	}
	if changed || !maps.Equal(cur.Annotations, ann) || !maps.Equal(cur.Labels, full.Labels) {
		cur.Annotations = ann
		cur.Labels = full.Labels
		if err := r.Update(ctx, cur); err != nil {
			return res, err
		}
	}

	ready := cur.Status.ReadyReplicas >= 1
	switch {
	case room.Spec.Paused:
		st.Phase = v1.RoomPaused
		setCond(st, gen, v1.ConditionPodReady, false, "Paused", "the room is paused; its volumes stay")
	case cur.Status.UpdateRevision != "" && cur.Status.CurrentRevision != cur.Status.UpdateRevision:
		st.Phase = v1.RoomUpdating
		setCond(st, gen, v1.ConditionPodReady, ready, "Updating", "rolling to a new pod template")
	case ready:
		st.Phase = v1.RoomRunning
		setCond(st, gen, v1.ConditionPodReady, true, "Ready", "")
	default:
		st.Phase = v1.RoomProvisioning
		setCond(st, gen, v1.ConditionPodReady, false, "NotReady", "waiting for the pod")
		if res.RequeueAfter == 0 {
			res.RequeueAfter = 30 * time.Second
		}
	}
	return res, nil
}

func (r *Reconciler) noteRoll(ws string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastRoll == nil {
		r.lastRoll = map[string]time.Time{}
	}
	r.lastRoll[ws] = r.now()
}

// mayRollAutomatically applies the workspace's update policy, and lets only
// one room per workspace roll at a time.
func (r *Reconciler) mayRollAutomatically(ctx context.Context, room *v1.Room, ws *v1.Workspace) bool {
	switch ws.Spec.UpdatePolicy.Mode {
	case v1.UpdateManual:
		return false
	case v1.UpdateWindow:
		if !InWindow(ws.Spec.UpdatePolicy.Window, r.now()) {
			return false
		}
	default:
		idle, err := r.idleChecker().Idle(ctx, ws.Name, room.Name)
		if err != nil || !idle {
			return false
		}
	}
	r.mu.Lock()
	last, ok := r.lastRoll[ws.Name]
	r.mu.Unlock()
	if ok && r.now().Sub(last) < 2*time.Minute {
		return false
	}
	var list appsv1.StatefulSetList
	if err := r.List(ctx, &list, client.InNamespace(room.Namespace), client.MatchingLabels{v1.LabelComponent: ComponentRoom}); err != nil {
		return false
	}
	for _, s := range list.Items {
		if s.Name == Name(room.Name) {
			continue
		}
		if s.Status.UpdateRevision != "" && s.Status.CurrentRevision != s.Status.UpdateRevision {
			return false
		}
	}
	return true
}

func (r *Reconciler) idleChecker() IdleChecker {
	if r.Idle == nil {
		return NeverIdle{}
	}
	return r.Idle
}

// InWindow reports whether t (UTC) is in a window like "Sat 02:00-06:00".
// A window may cross midnight ("Sun 23:00-01:00").
func InWindow(window string, t time.Time) bool {
	f := strings.Fields(window)
	if len(f) != 2 {
		return false
	}
	days := map[string]time.Weekday{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	day, ok := days[strings.ToLower(f[0])[:min(3, len(f[0]))]]
	if !ok {
		return false
	}
	from, to, ok := strings.Cut(f[1], "-")
	if !ok {
		return false
	}
	pf, err1 := time.Parse("15:04", from)
	pt, err2 := time.Parse("15:04", to)
	if err1 != nil || err2 != nil {
		return false
	}
	t = t.UTC()
	start := time.Date(t.Year(), t.Month(), t.Day(), pf.Hour(), pf.Minute(), 0, 0, time.UTC)
	start = start.AddDate(0, 0, int(day)-int(t.Weekday()))
	dur := pt.Sub(pf)
	if dur <= 0 {
		dur += 24 * time.Hour
	}
	for _, s := range []time.Time{start, start.AddDate(0, 0, -7)} {
		if !t.Before(s) && t.Before(s.Add(dur)) {
			return true
		}
	}
	return false
}

// ---- deletion and volume retention ----

func (r *Reconciler) finalize(ctx context.Context, room *v1.Room, wsName string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(room, Finalizer) {
		return ctrl.Result{}, nil
	}
	if room.Status.ForgeUser != "" {
		if r.Deps.Forge == nil {
			return ctrl.Result{}, errors.New("cannot disable forge user: no forge client")
		}
		fc, err := r.Deps.Forge(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("disable forge user: %w", err)
		}
		if err := fc.DisableUser(ctx, room.Status.ForgeUser); err != nil {
			return ctrl.Result{}, err
		}
	}
	var sec corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: room.Namespace, Name: IdentitySecretName(room.Name)}, &sec); err == nil {
		// Messaging refuses the removed room's certificate until it expires.
		if err := r.revokeCert(ctx, sec.Data[KeyTLSCert]); err != nil {
			return ctrl.Result{}, fmt.Errorf("revoke messaging certificate: %w", err)
		}
		var keys []string
		for _, k := range []string{KeyGatewayKey, KeyAdvisorKey, KeyWardenKey} {
			if v := string(sec.Data[k]); v != "" {
				keys = append(keys, v)
			}
		}
		if len(keys) > 0 && r.Deps.Gateway != nil {
			gc, err := r.Deps.Gateway(ctx)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("delete gateway key: %w", err)
			}
			if err := gc.DeleteKeys(ctx, keys...); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if err := r.removeTables(ctx, wsName, room.Name); err != nil {
		return ctrl.Result{}, err
	}
	retention := defaultDeletedRetention
	var ws v1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: wsName}, &ws); err == nil && ws.Spec.DeletedRoomRetention != nil {
		retention = ws.Spec.DeletedRoomRetention.Duration
	}
	if room.Annotations[AnnotationDeleteDataNow] != "" {
		retention = 0
	}
	deadline := r.now().Add(retention).UTC().Format(time.RFC3339)
	pvcs, err := r.roomPVCs(ctx, room.Namespace, room.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range pvcs {
		p := &pvcs[i]
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[AnnotationDeleteAfter] = deadline
		if err := r.Update(ctx, p); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(room, Finalizer)
	if err := r.Update(ctx, room); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: retention + time.Second}, nil
}

func (r *Reconciler) roomPVCs(ctx context.Context, ns, room string) ([]corev1.PersistentVolumeClaim, error) {
	var l corev1.PersistentVolumeClaimList
	err := r.List(ctx, &l, client.InNamespace(ns), client.MatchingLabels{v1.LabelRoom: room, v1.LabelComponent: ComponentRoom})
	return l.Items, err
}

// cleanupVolumes deletes a removed room's volumes once their retention has passed.
func (r *Reconciler) cleanupVolumes(ctx context.Context, ns, room string) (ctrl.Result, error) {
	pvcs, err := r.roomPVCs(ctx, ns, room)
	if err != nil {
		return ctrl.Result{}, err
	}
	var next time.Duration
	for i := range pvcs {
		p := &pvcs[i]
		at, err := time.Parse(time.RFC3339, p.Annotations[AnnotationDeleteAfter])
		if err != nil {
			continue // not marked: belongs to a room being created, or kept by hand
		}
		if wait := at.Sub(r.now()); wait > 0 {
			if next == 0 || wait < next {
				next = wait
			}
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// keepVolumes unmarks the volumes of a room that was re-created within its
// retention, so it gets its data back.
func (r *Reconciler) keepVolumes(ctx context.Context, room *v1.Room) {
	pvcs, err := r.roomPVCs(ctx, room.Namespace, room.Name)
	if err != nil {
		return
	}
	for i := range pvcs {
		p := &pvcs[i]
		if _, ok := p.Annotations[AnnotationDeleteAfter]; ok {
			delete(p.Annotations, AnnotationDeleteAfter)
			_ = r.Update(ctx, p)
		}
	}
}
