package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

func TestControlCenterOwnedRole(t *testing.T) {
	if r := ControlCenterOwnedRole(nil, nil); len(r.Rules) != 0 {
		t.Fatalf("no names must mean no rules (an empty resourceNames grants everything): %+v", r.Rules)
	}
	r := ControlCenterOwnedRole(
		[]string{"oidc-b", "platform-ca", "oidc-a", "oidc-a", "control-center-session", "notify-", "forge-webhook-acme"},
		[]string{"platform-ca-bundle", "saml-okta"})
	if len(r.Rules) != 2 {
		t.Fatalf("rules %+v", r.Rules)
	}
	if got := r.Rules[0]; got.Resources[0] != "secrets" || !slices.Equal(got.ResourceNames, []string{"forge-webhook-acme", "oidc-a", "oidc-b"}) ||
		!slices.Equal(got.Verbs, []string{"get", "update", "patch"}) {
		t.Errorf("secrets rule %+v", got)
	}
	if got := r.Rules[1]; got.Resources[0] != "configmaps" || !slices.Equal(got.ResourceNames, []string{"saml-okta"}) {
		t.Errorf("configmaps rule %+v", got)
	}
}

func TestControlCenterAccessReconcile(t *testing.T) {
	env := testenv.Start(t)
	c := env.Client
	ctx := context.Background()
	sys := v1alpha1.SystemNamespace
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: sys}}); err != nil {
		t.Fatal(err)
	}
	labelled := func(component string) map[string]string { return map[string]string{v1alpha1.LabelComponent: component} }
	for _, o := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oidc-google", Namespace: sys, Labels: labelled("control-center")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "forge-webhook-acme", Namespace: sys, Labels: labelled("forge-webhook")}},
		// Labelled, but not a control center name: never granted.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "platform-ca", Namespace: sys, Labels: labelled("control-center")}},
		// A control center name without the label: not granted either.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gateway-provider-x", Namespace: sys}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "saml-okta", Namespace: sys, Labels: labelled("control-center")}},
	} {
		if err := c.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	r := &ControlCenterAccess{Client: env.OperatorClient(t), Scheme: env.Scheme, Version: "v0.1.0"}
	reconcile := func() *rbacv1.Role {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
			t.Fatal(err)
		}
		var role rbacv1.Role
		if err := c.Get(ctx, types.NamespacedName{Namespace: sys, Name: v1alpha1.ControlCenterOwnedRole}, &role); err != nil {
			t.Fatal(err)
		}
		return &role
	}
	role := reconcile()
	if len(role.Rules) != 2 || !slices.Equal(role.Rules[0].ResourceNames, []string{"forge-webhook-acme", "oidc-google"}) ||
		!slices.Equal(role.Rules[1].ResourceNames, []string{"saml-okta"}) {
		t.Fatalf("rules %+v", role.Rules)
	}
	var rb rbacv1.RoleBinding
	if err := c.Get(ctx, types.NamespacedName{Namespace: sys, Name: v1alpha1.ControlCenterOwnedRole}, &rb); err != nil {
		t.Fatal(err)
	}
	if rb.RoleRef.Name != v1alpha1.ControlCenterOwnedRole || rb.Subjects[0].Name != "control-center" || rb.Subjects[0].Namespace != sys {
		t.Fatalf("binding %+v", rb)
	}

	// Deleting the objects takes the names away, down to no rule at all.
	for _, o := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oidc-google", Namespace: sys}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "forge-webhook-acme", Namespace: sys}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "saml-okta", Namespace: sys}},
	} {
		if err := c.Delete(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	if role := reconcile(); len(role.Rules) != 0 {
		t.Fatalf("rules after deletes %+v", role.Rules)
	}
}

func TestControlCenterGatewayKey(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/user/new" || body["user_role"] != gateway.RoleProxyAdminViewer || r.Header.Get("Authorization") != "Bearer sk-master" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		calls = append(calls, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]string{"key": "sk-viewer"})
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	scheme := testenv.Scheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: c, Scheme: scheme, Deps: deps.Deps{Gateway: func(context.Context) (*gateway.Client, error) {
		return gateway.New(srv.URL, "sk-master"), nil
	}}}
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName, UID: "p"}}
	r.ensureControlCenterGatewayKey(ctx, p)
	r.ensureControlCenterGatewayKey(ctx, p)
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: ControlCenterGatewaySecret}, &s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["masterKey"]) != "sk-viewer" || len(calls) != 1 || !v1alpha1.ControlCenterSecret(s.Name) {
		t.Fatalf("secret %v after %d calls", s.Data, len(calls))
	}
}
