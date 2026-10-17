package platform

import (
	"context"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/apply"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
)

// ControlCenterAccess keeps Role control-center-owned in the system
// namespace (design §11.3). The control center may create any Secret or
// ConfigMap there, but read and change only its own: the chart's Role names
// the fixed ones, and this Role names, one by one, the existing objects
// with a per-provider, per-channel or per-workspace name, since RBAC can't
// match a prefix or a label. An object is listed only when it is labelled
// for the control center and its name has one of the control center's
// prefixes (v1alpha1.ControlCenterPrefixedSecret), so a label alone never
// grants the platform CA or another operator Secret.
type ControlCenterAccess struct {
	client.Client
	Scheme  *runtime.Scheme
	Version string
}

// Components whose system-namespace Secrets the control center may be
// granted: what it creates itself, and the workspace webhook secrets the
// workspace controller makes for it (forge-webhook-<ws>).
var controlCenterComponents = []string{"control-center", "forge-webhook"}

// controlCenterServiceAccount is the chart's ServiceAccount for the control center.
const controlCenterServiceAccount = "control-center"

// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch

// SetupControlCenterAccess registers the controller.
func SetupControlCenterAccess(mgr ctrl.Manager, d deps.Deps) error {
	r := &ControlCenterAccess{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Version: d.Version}
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: v1alpha1.ControlCenterOwnedRole}}}
	})
	labelled := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == v1alpha1.SystemNamespace && slices.Contains(controlCenterComponents, o.GetLabels()[v1alpha1.LabelComponent])
	}))
	return ctrl.NewControllerManagedBy(mgr).
		Named("control-center-access").
		Watches(&corev1.Secret{}, one, labelled).
		Watches(&corev1.ConfigMap{}, one, labelled).
		Complete(r)
}

// Reconcile applies the Role and its RoleBinding for the objects there now.
func (r *ControlCenterAccess) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	sys := v1alpha1.SystemNamespace
	req, err := labels.NewRequirement(v1alpha1.LabelComponent, selection.In, controlCenterComponents)
	if err != nil {
		return ctrl.Result{}, err
	}
	sel := client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*req)}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(sys), sel); err != nil {
		return ctrl.Result{}, err
	}
	var cms corev1.ConfigMapList
	if err := r.List(ctx, &cms, client.InNamespace(sys), sel); err != nil {
		return ctrl.Result{}, err
	}
	var sn, cn []string
	for _, s := range secrets.Items {
		sn = append(sn, s.Name)
	}
	for _, c := range cms.Items {
		cn = append(cn, c.Name)
	}
	a := &apply.Applier{Client: r.Client, Scheme: r.Scheme, Version: r.Version}
	if err := a.ApplyAll(ctx, nil, []client.Object{ControlCenterOwnedRole(sn, cn), controlCenterOwnedBinding()}); err != nil {
		return ctrl.Result{}, err
	}
	// Repairs edits made by hand; changes to the objects trigger sooner.
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// ControlCenterOwnedRole is Role control-center-owned for these Secrets and
// ConfigMaps: get, update and patch by name. Names without one of the
// control center's prefixes are left out, and a kind with no names gets no
// rule (a rule with no resourceNames would cover every object).
func ControlCenterOwnedRole(secrets, configMaps []string) *rbacv1.Role {
	verbs := []string{"get", "update", "patch"}
	rules := []rbacv1.PolicyRule{}
	if names := grantable(secrets, v1alpha1.ControlCenterPrefixedSecret); len(names) > 0 {
		rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: names, Verbs: verbs})
	}
	if names := grantable(configMaps, v1alpha1.ControlCenterPrefixedConfigMap); len(names) > 0 {
		rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: names, Verbs: verbs})
	}
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ControlCenterOwnedRole, Namespace: v1alpha1.SystemNamespace,
			Labels: map[string]string{v1alpha1.LabelComponent: "control-center"}},
		Rules: rules,
	}
}

func controlCenterOwnedBinding() *rbacv1.RoleBinding {
	sys := v1alpha1.SystemNamespace
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ControlCenterOwnedRole, Namespace: sys,
			Labels: map[string]string{v1alpha1.LabelComponent: "control-center"}},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: v1alpha1.ControlCenterOwnedRole},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: controlCenterServiceAccount, Namespace: sys}},
	}
}

// grantable is the sorted, unique names that ok accepts.
func grantable(names []string, ok func(string) bool) []string {
	var out []string
	for _, n := range names {
		if ok(n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
