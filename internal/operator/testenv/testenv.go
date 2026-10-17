// Package testenv starts an envtest API server with swarmageddon's CRDs for
// controller tests. Tests skip when KUBEBUILDER_ASSETS is unset; `make
// envtest` downloads the binaries and prints the path.
package testenv

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Env is a running API server.
type Env struct {
	Config *rest.Config
	Client client.Client
	Scheme *k8sruntime.Scheme
	Mapper meta.RESTMapper
}

// Scheme has client-go's types and ours.
func Scheme() *k8sruntime.Scheme {
	s := k8sruntime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

// Start starts an API server for the test (stopped on cleanup), or skips.
func Start(t *testing.T) *Env {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make envtest` (see Makefile)")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(root, "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := Scheme()
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	return &Env{Config: cfg, Client: c, Scheme: scheme, Mapper: mapper}
}

// OperatorClient binds config/rbac/role.yaml (the generated ClusterRole) to
// a ServiceAccount and returns a client impersonating it, so a test fails
// when the operator lacks a permission, including RBAC escalation checks.
func (e *Env) OperatorClient(t *testing.T) client.Client {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(data, &role); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.Client.Create(ctx, &role); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	user := "system:serviceaccount:" + v1alpha1.SystemNamespace + ":swarmageddon-operator"
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "swarmageddon-operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
	}
	if err := e.Client.Create(ctx, binding); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	cfg := rest.CopyConfig(e.Config)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:serviceaccounts", "system:authenticated"}}
	c, err := client.New(cfg, client.Options{Scheme: e.Scheme, Mapper: e.Mapper})
	if err != nil {
		t.Fatal(err)
	}
	// The authorizer sees a new binding a moment later; until then the
	// operator is forbidden everything and tests fail at random.
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := c.List(ctx, &v1alpha1.PlatformList{})
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("the operator's RBAC never took effect: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
