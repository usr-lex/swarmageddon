package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
)

// TestProtectOtherRepos: repositories made in Forgejo itself get the
// default branch's and v* tags' protection on the resync; the workspace's
// own, archived ones and mirrors are left to their own handling.
func TestProtectOtherRepos(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		call := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/v1")
		calls = append(calls, call)
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies[call] = b
		w.Header().Set("Content-Type", "application/json")
		switch {
		case call == "GET /orgs/acme/repos":
			_ = json.NewEncoder(w).Encode([]forgejo.Repository{
				{Name: "api", DefaultBranch: "main"},
				{Name: "notes", DefaultBranch: "trunk"},
				{Name: "old", DefaultBranch: "main", Archived: true},
				{Name: "upstream", DefaultBranch: "main", Mirror: true},
			})
		case r.Method == http.MethodGet && strings.Contains(call, "/branch_protections/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte("[]"))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	defer srv.Close()

	r := &Reconciler{}
	w := &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1alpha1.WorkspaceSpec{
		Forge:        v1alpha1.ForgeSpec{Org: "acme", RequiredApprovals: 2},
		Repositories: []v1alpha1.RepositorySpec{{Name: "API"}},
	}}
	n, err := r.protectOtherRepos(context.Background(), w, forgejo.New(srv.URL, "t"))
	if err != nil || n != 1 {
		t.Fatalf("protected %d, %v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	bp := bodies["POST /repos/acme/notes/branch_protections"]
	if bp == nil || bp["rule_name"] != "trunk" || bp["required_approvals"] != float64(2) || bp["enable_push"] != false ||
		bp["require_signed_commits"] != true {
		t.Fatalf("protection = %v (calls %q)", bp, calls)
	}
	if bodies["POST /repos/acme/notes/tag_protections"]["name_pattern"] != "v*" {
		t.Errorf("no tag protection: %q", calls)
	}
	for _, c := range calls {
		for _, other := range []string{"/repos/acme/api/", "/repos/acme/old/", "/repos/acme/upstream/"} {
			if strings.Contains(c, other) {
				t.Errorf("touched %s", c)
			}
		}
	}
	if !slices.Contains(calls, "GET /orgs/acme/repos") {
		t.Error("never listed the org")
	}
}
