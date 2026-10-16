// Package testenv starts an envtest API server with swarmageddon's CRDs for
// controller tests. Tests skip when KUBEBUILDER_ASSETS is unset; `make
// envtest` downloads the binaries and prints the path.
package testenv

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

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
