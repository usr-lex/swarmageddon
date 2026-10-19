package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
)

// ConditionGitHubHistory reports whether imported GitHub history is
// attributed to the rooms (ADR 0012).
const ConditionGitHubHistory = "GitHubHistoryLinked"

// annGitHubLinks records the accounts last linked, so the database is only
// written when they change.
const annGitHubLinks = "swarmageddon.dev/github-links"

// githubSource is the inactive OAuth2 auth source the links belong to. It
// offers no sign-in button; Forgejo only needs it as the links' source.
const githubSource = "github-import"

// linkGitHubAccounts makes each mapped GitHub account's imported posts its
// room's own. Forgejo keeps an imported post's GitHub author ID and
// re-attributes it to a local user linked to that ID (cron task
// update_migration_poster_id), but has no API to create the link, so the
// operator writes it into Forgejo's database (ADR 0012). It waits
// (deps.ErrNotReady) until every room's forge user exists.
func (r *Reconciler) linkGitHubAccounts(ctx context.Context, w *v1alpha1.Workspace) error {
	accounts := w.Spec.GitHubAccounts
	if len(accounts) == 0 {
		return nil
	}
	want := linksHash(w)
	if w.Annotations[annGitHubLinks] == want {
		return nil
	}
	if r.Deps.Forge == nil || r.Deps.Exec == nil {
		return fmt.Errorf("%w: no forge client or pod exec", deps.ErrNotReady)
	}
	fc, err := r.Deps.Forge(ctx)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if _, err := fc.GetUser(ctx, a.Room); err != nil {
			if forgejo.IsNotFound(err) {
				return fmt.Errorf("%w: room %s has no forge user yet", deps.ErrNotReady, a.Room)
			}
			return err
		}
	}
	ns := v1alpha1.PlatformNamespace
	var out, errOut bytes.Buffer
	if err := r.Deps.Exec.Exec(ctx, ns, "forgejo-0", "forgejo", []string{"forgejo", "admin", "auth", "list"}, nil, &out, &errOut); err != nil {
		return fmt.Errorf("forgejo admin auth list: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	if !hasAuthSource(out.String(), githubSource) {
		errOut.Reset()
		cmd := []string{"forgejo", "admin", "auth", "add-oauth", "--name", githubSource, "--provider", "github", "--key", "unused", "--secret", "unused"}
		if err := r.Deps.Exec.Exec(ctx, ns, "forgejo-0", "forgejo", cmd, nil, &out, &errOut); err != nil {
			return fmt.Errorf("forgejo admin auth add-oauth: %w: %s", err, strings.TrimSpace(errOut.String()))
		}
	}
	errOut.Reset()
	psql := []string{"psql", "-U", "postgres", "-d", "forgejo", "-v", "ON_ERROR_STOP=1"}
	if err := r.Deps.Exec.Exec(ctx, ns, "postgres-0", "postgres", psql, strings.NewReader(linkSQL(accounts)), &out, &errOut); err != nil {
		return fmt.Errorf("linking GitHub accounts: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	if err := fc.RunCron(ctx, "update_migration_poster_id"); err != nil {
		return fmt.Errorf("re-attributing imported posts: %w", err)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{annGitHubLinks: want}}})
	return r.Patch(ctx, &v1alpha1.Workspace{ObjectMeta: w.ObjectMeta}, client.RawPatch(types.MergePatchType, patch))
}

// linkSQL keeps the auth source inactive (no sign-in button) and links each
// account; existing links stay.
func linkSQL(accounts []v1alpha1.GitHubAccount) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	var b strings.Builder
	b.WriteString("BEGIN;\nUPDATE login_source SET is_active = false WHERE name = " + q(githubSource) + ";\n")
	for _, a := range accounts {
		fmt.Fprintf(&b, "INSERT INTO external_login_user (external_id, user_id, login_source_id, provider, name) "+
			"SELECT %s, u.id, s.id, 'github', %s FROM \"user\" u, login_source s "+
			"WHERE u.lower_name = %s AND s.name = %s ON CONFLICT DO NOTHING;\n",
			q(strconv.FormatInt(a.ID, 10)), q(a.Login), q(strings.ToLower(a.Room)), q(githubSource))
	}
	b.WriteString("COMMIT;\n")
	return b.String()
}

// linksHash covers the accounts and the GitHub imports, so a newly imported
// repository gets its posts re-attributed too.
func linksHash(w *v1alpha1.Workspace) string {
	var parts []string
	for _, a := range w.Spec.GitHubAccounts {
		parts = append(parts, fmt.Sprintf("%s=%d=%s", a.Login, a.ID, a.Room))
	}
	for _, r := range w.Spec.Repositories {
		if r.ImportGitHub {
			parts = append(parts, "repo="+r.Name)
		}
	}
	slices.Sort(parts)
	return manifests.Hash(strings.Join(parts, "\n"))
}

// hasAuthSource finds name in `forgejo admin auth list` (tab-separated
// rows of ID, name, type and enabled after a header).
func hasAuthSource(list, name string) bool {
	for i, line := range strings.Split(list, "\n") {
		if i == 0 {
			continue
		}
		if f := strings.Split(line, "\t"); len(f) > 1 && f[1] == name {
			return true
		}
	}
	return false
}
