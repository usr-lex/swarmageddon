package rbactest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// installObjects returns deploy/install.yaml's objects of these kinds.
func installObjects(t *testing.T, kinds ...string) []*unstructured.Unstructured {
	t.Helper()
	objs, err := manifests.Decode(repoFile(t, "deploy/install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if slices.Contains(kinds, o.GetKind()) {
			out = append(out, o)
		}
	}
	return out
}

// sa is a ServiceAccount as the API server authenticates it.
type sa struct{ namespace, name string }

func (s sa) user() string { return "system:serviceaccount:" + s.namespace + ":" + s.name }

func (s sa) groups() []string {
	return []string{"system:serviceaccounts", "system:serviceaccounts:" + s.namespace, "system:authenticated"}
}

// config is cfg impersonating the ServiceAccount.
func (s sa) config(cfg *rest.Config) *rest.Config {
	c := rest.CopyConfig(cfg)
	c.Impersonate = rest.ImpersonationConfig{UserName: s.user(), Groups: s.groups()}
	return c
}

// call is one API request: verb on group/resource[/subresource] in a
// namespace (empty: cluster-scoped or all), optionally one object by name.
type call struct {
	verb, group, resource, sub, ns, name string
}

func (c call) String() string {
	s := c.verb + " " + c.resource
	if c.group != "" {
		s += "." + c.group
	}
	if c.sub != "" {
		s += "/" + c.sub
	}
	if c.ns != "" {
		s += " in " + c.ns
	}
	if c.name != "" {
		s += " named " + c.name
	}
	return s
}

// allowed asks the API server's authorizer, as admin, whether s may make c.
func (s sa) allowed(t *testing.T, admin client.Client, c call) bool {
	t.Helper()
	r := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: s.user(), Groups: s.groups(),
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: c.ns, Verb: c.verb, Group: c.group, Resource: c.resource, Subresource: c.sub, Name: c.name,
		},
	}}
	if err := admin.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r.Status.Allowed
}

// expect checks every call: allowed or refused.
func (s sa) expect(t *testing.T, admin client.Client, want bool, calls ...call) {
	t.Helper()
	for _, c := range calls {
		if got := s.allowed(t, admin, c); got != want {
			if want {
				t.Errorf("%s may not %s", s.name, c)
			} else {
				t.Errorf("%s may %s", s.name, c)
			}
		}
	}
}
