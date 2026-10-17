package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestRepoCredentialSecretName(t *testing.T) {
	a, b := RepoCredentialSecretName("My_Repo.v2", "mirror"), RepoCredentialSecretName("My_Repo.v2", "mirror")
	if a == b || !strings.HasPrefix(a, "repo-my-repo-v2-mirror-") {
		t.Fatalf("names %q %q", a, b)
	}
	for _, n := range []string{a, RepoCredentialSecretName(strings.Repeat("x", 300), "import")} {
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			t.Errorf("%q: %v", n, errs)
		}
	}
}

func TestCreateRepoCredential(t *testing.T) {
	ctx := context.Background()
	// The first create finds no namespace, as before the operator made it.
	calls := 0
	c := fake.NewClientBuilder().WithScheme(Scheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			calls++
			if calls == 1 {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, obj.GetNamespace())
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	kc := &Client{C: c, Namespace: v1alpha1.SystemNamespace}
	if err := kc.CreateRepoCredential(ctx, "acme", "repo-api-import-x", "api", "import", "bot", "ghp_1", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := kc.CreateRepoCredential(ctx, "acme", "repo-api-mirror-y", "api", "mirror", "", "ghp_2", time.Second); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "repo-api-import-x"}, &s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["token"]) != "ghp_1" || string(s.Data["username"]) != "bot" || s.Labels[v1alpha1.LabelComponent] != RepoCredentialComponent {
		t.Fatalf("import secret %+v", s)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "repo-api-mirror-y"}, &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Data["username"]; ok || string(s.Data["token"]) != "ghp_2" {
		t.Fatalf("mirror secret %v", s.Data)
	}
	// A name that exists is refused, not overwritten.
	if err := kc.CreateRepoCredential(ctx, "acme", "repo-api-mirror-y", "api", "mirror", "", "other", time.Second); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("overwrite: %v", err)
	}
	// No namespace at all: the wait ends.
	calls = 0
	if err := kc.CreateRepoCredential(ctx, "acme", "repo-x-import-z", "x", "import", "", "t", 0); err != ErrWorkspaceNamespaceMissing {
		t.Fatalf("no namespace: %v", err)
	}
}
