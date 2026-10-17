package rbactest

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/platform"
	"github.com/usr-lex/swarmageddon/internal/operator/workspace"
)

// grantedInGo are the Go functions that build Roles the operator applies,
// by file. A new rbacv1.Role or ClusterRole literal elsewhere in
// internal/operator fails TestEveryGrantIsListed until it is added here.
var grantedInGo = map[string]func() []rbacv1.PolicyRule{
	"workspace/controller.go": func() []rbacv1.PolicyRule { return workspace.ControlCenterRole("ws-acme").Rules },
	"platform/ccaccess.go": func() []rbacv1.PolicyRule {
		return platform.ControlCenterOwnedRole([]string{"oidc-google", "notify-acme-slack"}, []string{"saml-okta"}).Rules
	},
}

// TestOperatorHoldsWhatItGrants checks every rule of every Role and
// ClusterRole the operator applies against its own ClusterRole
// (config/rbac/role.yaml): Kubernetes refuses to let it grant more.
func TestOperatorHoldsWhatItGrants(t *testing.T) {
	var operator rbacv1.ClusterRole
	if err := yaml.Unmarshal(repoFile(t, "config/rbac/role.yaml"), &operator); err != nil {
		t.Fatal(err)
	}
	granted := map[string][]rbacv1.PolicyRule{}
	for file, rules := range grantedInGo {
		granted[file] = rules()
	}
	// The platform templates, as rendered with test values (TestGolden in
	// internal/operator/manifests keeps them current).
	goldens, err := filepath.Glob(filepath.Join(operatorDir(), "manifests", "testdata", "*.golden.yaml"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("goldens: %v", err)
	}
	for _, g := range goldens {
		b, err := os.ReadFile(g)
		if err != nil {
			t.Fatal(err)
		}
		objs, err := manifests.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			if o.GetKind() != "Role" && o.GetKind() != "ClusterRole" {
				continue
			}
			var r rbacv1.ClusterRole
			if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &r); err != nil {
				t.Fatal(err)
			}
			granted[filepath.Base(g)+" "+o.GetKind()+" "+o.GetName()] = r.Rules
		}
	}
	if len(granted) < 6 {
		t.Fatalf("found only %d granted roles", len(granted))
	}
	for what, rules := range granted {
		for _, r := range rules {
			for _, missing := range uncovered(operator.Rules, r) {
				t.Errorf("%s grants %s, which the operator doesn't hold (add a +kubebuilder:rbac marker, make manifests)", what, missing)
			}
		}
	}
}

// TestChartNamesTheControlCenterSecrets checks the chart's Role
// control-center against the allowlist the control center checks names with.
func TestChartNamesTheControlCenterSecrets(t *testing.T) {
	var found bool
	for _, o := range installObjects(t, "Role") {
		if o.GetName() != "control-center" {
			continue
		}
		found = true
		var r rbacv1.Role
		if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &r); err != nil {
			t.Fatal(err)
		}
		named := map[string][]string{}
		for _, rule := range r.Rules {
			if len(rule.ResourceNames) > 0 {
				named[rule.Resources[0]] = append(named[rule.Resources[0]], rule.ResourceNames...)
			}
		}
		for res, want := range map[string][]string{"secrets": v1alpha1.ControlCenterSecrets, "configmaps": v1alpha1.ControlCenterConfigMaps} {
			got := slices.Sorted(slices.Values(named[res]))
			if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
				t.Errorf("chart Role control-center names %s %v; api/v1alpha1 lists %v", res, got, want)
			}
		}
	}
	if !found {
		t.Fatal("no Role control-center in deploy/install.yaml")
	}
}

// TestEveryGrantIsListed finds the Role literals in the operator's code.
func TestEveryGrantIsListed(t *testing.T) {
	root := operatorDir()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if (strings.Contains(string(b), "rbacv1.Role{") || strings.Contains(string(b), "rbacv1.ClusterRole{")) && grantedInGo[filepath.ToSlash(rel)] == nil {
			t.Errorf("%s builds a Role; add it to grantedInGo", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUncovered(t *testing.T) {
	owner := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "update"}},
		{APIGroups: []string{"apps"}, Resources: []string{"*"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"a"}, Verbs: []string{"get"}},
	}
	for _, tc := range []struct {
		r    rbacv1.PolicyRule
		want int
	}{
		{rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"x"}, Verbs: []string{"get"}}, 0},
		{rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets", "pods"}, Verbs: []string{"get", "patch"}}, 3},
		{rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get"}}, 0},
		{rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"a", "b"}, Verbs: []string{"get"}}, 1},
		{rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}, 1},
	} {
		if got := uncovered(owner, tc.r); len(got) != tc.want {
			t.Errorf("%+v: uncovered %v", tc.r, got)
		}
	}
}

// uncovered lists what rule r allows that owner's rules don't, one
// "verb group/resource[ name]" each.
func uncovered(owner []rbacv1.PolicyRule, r rbacv1.PolicyRule) []string {
	names := r.ResourceNames
	if len(names) == 0 {
		names = []string{""}
	}
	var out []string
	for _, g := range r.APIGroups {
		for _, res := range r.Resources {
			for _, v := range r.Verbs {
				for _, n := range names {
					if !covers(owner, g, res, v, n) {
						out = append(out, strings.TrimSpace(v+" "+g+"/"+res+" "+n))
					}
				}
			}
		}
	}
	return out
}

func covers(owner []rbacv1.PolicyRule, group, resource, verb, name string) bool {
	has := func(l []string, v string) bool { return slices.Contains(l, v) || slices.Contains(l, "*") }
	for _, o := range owner {
		if has(o.APIGroups, group) && has(o.Resources, resource) && has(o.Verbs, verb) &&
			(len(o.ResourceNames) == 0 || name != "" && slices.Contains(o.ResourceNames, name)) {
			return true
		}
	}
	return false
}

// operatorDir is internal/operator.
func operatorDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..")
}
