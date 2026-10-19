package workspace

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
)

type recExec struct {
	mu    sync.Mutex
	calls []string // pod/container: command (and stdin)
	list  string   // forgejo admin auth list output
}

func (e *recExec) Exec(_ context.Context, _, pod, container string, cmd []string, stdin io.Reader, stdout, _ io.Writer) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	e.calls = append(e.calls, pod+"/"+container+": "+strings.Join(cmd, " ")+" <<"+in)
	if slices.Contains(cmd, "list") {
		_, _ = io.WriteString(stdout, e.list)
	}
	return nil
}

// Imported GitHub history becomes the rooms' own (ADR 0012): once the rooms'
// forge users exist, the operator keeps an inactive GitHub auth source, links
// each account to its room's user in Forgejo's database, and runs Forgejo's
// re-attribution (verified by hand on Forgejo 17 with Postgres).
func TestLinkGitHubAccounts(t *testing.T) {
	var mu sync.Mutex
	users := map[string]bool{"athena": true}
	crons := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/users/"):
			if !users[strings.TrimPrefix(r.URL.Path, "/api/v1/users/")] {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `{"message":"not found"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":2,"login":"x"}`)
		case r.Method == "POST" && r.URL.Path == "/api/v1/admin/cron/update_migration_poster_id":
			crons++
			w.WriteHeader(204)
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	ws := &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1alpha1.WorkspaceSpec{
		Forge: v1alpha1.ForgeSpec{Org: "acme"},
		GitHubAccounts: []v1alpha1.GitHubAccount{
			{Login: "bot-athena", ID: 4242, Room: "athena"},
			{Login: "bot-o'brien", ID: 77, Room: "hermes"},
		},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ws).Build()
	ex := &recExec{list: "ID\tName\tType\tEnabled\n"}
	r := &Reconciler{Client: c, Scheme: s, Deps: deps.Deps{
		Forge: func(context.Context) (*forgejo.Client, error) { return forgejo.New(srv.URL, "admin"), nil },
		Exec:  ex,
	}}
	ctx := context.Background()

	// hermes has no forge user yet: wait, write nothing.
	if err := r.linkGitHubAccounts(ctx, ws); !errors.Is(err, deps.ErrNotReady) || len(ex.calls) != 0 || crons != 0 {
		t.Fatalf("before the users exist: %v, calls %q, crons %d", err, ex.calls, crons)
	}

	mu.Lock()
	users["hermes"] = true
	mu.Unlock()
	if err := r.linkGitHubAccounts(ctx, ws); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(ex.calls, "\n")
	for _, want := range []string{
		"forgejo-0/forgejo: forgejo admin auth add-oauth --name github-import --provider github",
		"postgres-0/postgres: psql -U postgres -d forgejo -v ON_ERROR_STOP=1",
		"UPDATE login_source SET is_active = false WHERE name = 'github-import'",
		"SELECT '4242', u.id",
		"lower_name = 'athena'",
		"'bot-o''brien'", // quoted
		"ON CONFLICT DO NOTHING",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("calls lack %q:\n%s", want, all)
		}
	}
	if crons != 1 {
		t.Errorf("re-attribution ran %d times", crons)
	}
	var got v1alpha1.Workspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(ws), &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[annGitHubLinks] == "" {
		t.Fatal("not recorded")
	}

	// Recorded: nothing runs again until the accounts change.
	ex.calls = nil
	if err := r.linkGitHubAccounts(ctx, &got); err != nil || len(ex.calls) != 0 || crons != 1 {
		t.Fatalf("ran again: %v %q %d", err, ex.calls, crons)
	}
}
