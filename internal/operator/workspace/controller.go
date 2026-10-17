// Package workspace reconciles Workspaces (docs/design.md §5): the ws-<name>
// namespace and its baseline policy, the control center's access to it, the
// built-in RoleTemplates, the Forgejo org with its teams, repositories,
// protection and webhook, and the workspace's LiteLLM team.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/apply"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/room"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// Condition types beyond the API package's.
const (
	ConditionRoleTemplatesReady = "RoleTemplatesReady"
	ConditionRepositoriesReady  = "RepositoriesReady"
	ConditionForgeWebhookReady  = "ForgeWebhookReady"
	ConditionGatewayTeamReady   = "GatewayTeamReady"
	// ConditionSharedVolumesValid is false when a shared volume would be
	// writable on both sides of a wall.
	ConditionSharedVolumesValid = "SharedVolumesValid"
)

// ControlCenterServiceAccount is bound to the per-workspace Role.
const ControlCenterServiceAccount = "control-center"

// WebhookSecretName is the Secret in the system namespace with the
// workspace's webhook HMAC secret (key "secret").
func WebhookSecretName(workspace string) string { return "forge-webhook-" + workspace }

// WebhookURL is where Forgejo delivers the workspace's events.
func WebhookURL(workspace string) string {
	return "https://control-center." + v1alpha1.SystemNamespace + ".svc/hooks/forgejo/" + workspace
}

// Reconciler reconciles Workspaces.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Deps   deps.Deps
}

// SetupWithManager registers the workspace controller.
func SetupWithManager(mgr ctrl.Manager, d deps.Deps) error {
	r := &Reconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Deps: d}
	byNamespace := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		ns := o.GetNamespace()
		if len(ns) <= len(v1alpha1.WorkspaceNamespacePrefix) || ns[:len(v1alpha1.WorkspaceNamespacePrefix)] != v1alpha1.WorkspaceNamespacePrefix {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ns[len(v1alpha1.WorkspaceNamespacePrefix):]}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("workspace").
		For(&v1alpha1.Workspace{}).
		Owns(&corev1.Namespace{}).
		Watches(&v1alpha1.Room{}, byNamespace).
		Watches(&v1alpha1.RoleTemplate{}, byNamespace).
		Complete(r)
}

func (r *Reconciler) applier() *apply.Applier {
	return &apply.Applier{Client: r.Client, Scheme: r.Scheme, Version: r.Deps.Version}
}

func setCond(w *v1alpha1.Workspace, typ string, ok bool, reason, msg string) {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: w.Generation})
}

// Reconcile brings one workspace up to date.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var w v1alpha1.Workspace
	if err := r.Get(ctx, req.NamespacedName, &w); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if w.DeletionTimestamp != nil {
		// The namespace and everything in it go with the owner reference.
		// The forge org stays, so history keeps its authors.
		return ctrl.Result{}, nil
	}
	orig := w.DeepCopy()
	res, err := r.reconcile(ctx, &w)
	w.Status.ObservedGeneration = w.Generation
	if perr := r.Status().Patch(ctx, &w, client.MergeFrom(orig)); perr != nil && err == nil {
		err = perr
	}
	return res, err
}

func (r *Reconciler) reconcile(ctx context.Context, w *v1alpha1.Workspace) (ctrl.Result, error) {
	ns := v1alpha1.WorkspaceNamespace(w.Name)
	w.Status.Namespace = ns
	if err := r.ensureNamespace(ctx, w, ns); err != nil {
		setCond(w, v1alpha1.ConditionNamespaceReady, false, "Error", err.Error())
		return ctrl.Result{}, err
	}
	setCond(w, v1alpha1.ConditionNamespaceReady, true, "Ready", "namespace "+ns+" with policies, quota and control center access")

	if err := r.ensureRoleTemplates(ctx, w, ns); err != nil {
		setCond(w, ConditionRoleTemplatesReady, false, "Error", err.Error())
		return ctrl.Result{}, err
	}
	setCond(w, ConditionRoleTemplatesReady, true, "Ready", "built-in role templates present")

	if err := r.reconcileChannels(ctx, w, ns); err != nil {
		setCond(w, ConditionChannelsValid, false, "Error", err.Error())
		return ctrl.Result{}, err
	}

	if err := r.checkSharedVolumes(ctx, w, ns); err != nil {
		return ctrl.Result{}, err
	}

	retry := false
	if err := r.reconcileForge(ctx, w); err != nil {
		if !errors.Is(err, deps.ErrNotReady) {
			ctrl.LoggerFrom(ctx).Error(err, "forge")
		}
		retry = true
	}
	if err := r.reconcileGateway(ctx, w, ns); err != nil {
		if !errors.Is(err, deps.ErrNotReady) {
			ctrl.LoggerFrom(ctx).Error(err, "gateway")
		}
		retry = true
	}
	if retry {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	// Periodic resync catches repositories and changes made outside the operator.
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// checkSharedVolumes sets SharedVolumesValid: false, naming the volumes,
// when a shared volume would be writable on both sides of a wall (design
// §6.6). The room controller mounts those read-only everywhere.
func (r *Reconciler) checkSharedVolumes(ctx context.Context, w *v1alpha1.Workspace, ns string) error {
	if len(w.Spec.SharedVolumes) == 0 {
		meta.RemoveStatusCondition(&w.Status.Conditions, ConditionSharedVolumesValid)
		return nil
	}
	var rooms v1alpha1.RoomList
	if err := r.List(ctx, &rooms, client.InNamespace(ns)); err != nil {
		return err
	}
	walls := room.WallViolations(w, rooms.Items)
	if len(walls) == 0 {
		setCond(w, ConditionSharedVolumesValid, true, "Valid", fmt.Sprintf("%d shared volumes, none writable across a wall", len(w.Spec.SharedVolumes)))
		return nil
	}
	var msgs []string
	for _, name := range slices.Sorted(maps.Keys(walls)) {
		msgs = append(msgs, walls[name])
	}
	setCond(w, ConditionSharedVolumesValid, false, "WallCrossed", strings.Join(msgs, "; "))
	return nil
}

// ensureNamespace applies the namespace and its baseline objects.
func (r *Reconciler) ensureNamespace(ctx context.Context, w *v1alpha1.Workspace, ns string) error {
	a := r.applier()
	if err := a.Apply(ctx, w, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{
		v1alpha1.LabelWorkspace: w.Name,
		// Rooms need capabilities and an unmasked /proc inside their user
		// namespace, which the baseline profile refuses; warn on it instead.
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/warn":    "baseline",
	}}}); err != nil {
		return err
	}
	if r.Deps.PKI == nil {
		return fmt.Errorf("no PKI")
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	dns := intstr.FromInt32(53)
	objs := []client.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "platform-ca-bundle", Namespace: ns},
			Data:       map[string]string{pki.CACertKey: string(r.Deps.PKI.CABundle())},
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "default-deny", Namespace: ns},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			},
		},
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "allow-dns", Namespace: ns},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
				}},
			},
		},
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "workspace", Namespace: ns},
			Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
				corev1.ResourcePods:                   resource.MustParse("200"),
				corev1.ResourcePersistentVolumeClaims: resource.MustParse("200"),
				corev1.ResourceRequestsStorage:        resource.MustParse("10Ti"),
				corev1.ResourceServices:               resource.MustParse("200"),
			}},
		},
		&corev1.LimitRange{
			ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: ns},
			Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type:           corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				Default:        corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			}}},
		},
		// Values for rooms' secretEnv and MCP valueFrom that people enter in
		// the control center (design §6.6). Created empty; the control center
		// writes the data, which this apply never owns.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: RoomSecretsName, Namespace: ns, Labels: map[string]string{
				v1alpha1.LabelWorkspace: w.Name, "swarmageddon.dev/room-secret": "true",
			}},
			Type: corev1.SecretTypeOpaque,
		},
		controlCenterRole(ns),
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "control-center", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "control-center"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: ControlCenterServiceAccount, Namespace: v1alpha1.SystemNamespace}},
		},
	}
	return a.ApplyAll(ctx, w, objs)
}

// controlCenterRole is the control center's access in a workspace (design §11.3).
func controlCenterRole(ns string) *rbacv1.Role {
	rw := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	read := []string{"get", "list", "watch"}
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "control-center", Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"rooms", "roletemplates", "runnerpools"}, Verbs: rw},
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"rooms/status", "runnerpools/status"}, Verbs: read},
			{APIGroups: []string{""}, Resources: []string{"pods", "persistentvolumeclaims", "events"}, Verbs: read},
			{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"pods/exec", "pods/portforward"}, Verbs: []string{"create"}},
			{APIGroups: []string{"metrics.k8s.io"}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			// Only the one Secret people fill in through the control center.
			{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{RoomSecretsName}, Verbs: []string{"get", "update", "patch"}},
			// Repository credentials from the wizard, each a new Secret
			// (docs/conventions.md). Create can't be limited by name, and
			// gives no read access.
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
		},
	}
}

// RoomSecretsName is the Secret in each workspace namespace that holds values
// the control center writes for rooms' secretEnv and MCP valueFrom.
const RoomSecretsName = "room-secrets"

// ensureRoleTemplates creates the built-in templates that are missing. It
// never overwrites a template, so people's edits stay.
func (r *Reconciler) ensureRoleTemplates(ctx context.Context, w *v1alpha1.Workspace, ns string) error {
	for _, t := range BuiltinRoleTemplates(w.Name, w.Spec.Forge.Org) {
		t.Namespace = ns
		if err := r.Create(ctx, &t); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("role template %s: %w", t.Name, err)
		}
	}
	return nil
}

// ensureWebhookSecret returns the workspace's webhook secret, creating it
// once, and whether it was just created.
func (r *Reconciler) ensureWebhookSecret(ctx context.Context, w *v1alpha1.Workspace) (string, bool, error) {
	key := types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: WebhookSecretName(w.Name)}
	var sec corev1.Secret
	err := r.Get(ctx, key, &sec)
	if err == nil && len(sec.Data["secret"]) > 0 {
		return string(sec.Data["secret"]), false, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return "", false, err
	}
	if err == nil {
		// Present but empty: fill it in.
		sec.Data = map[string][]byte{"secret": []byte(randomHex(32))}
		return string(sec.Data["secret"]), true, r.Update(ctx, &sec)
	}
	value := randomHex(32)
	sec = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{
			v1alpha1.LabelManaged: "true", v1alpha1.LabelWorkspace: w.Name, v1alpha1.LabelComponent: "forge-webhook",
		}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"secret": []byte(value)},
	}
	if err := controllerutil.SetControllerReference(w, &sec, r.Scheme); err != nil {
		return "", false, err
	}
	if err := r.Create(ctx, &sec); err != nil {
		return "", false, err
	}
	return value, true, nil
}
