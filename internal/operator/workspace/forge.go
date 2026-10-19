package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
)

// Built-in team names (design §9.1).
const (
	TeamImplementers = "implementers"
	TeamReviewers    = "reviewers"
	TeamMergers      = "mergers"
	TeamCI           = "ci"
	// TeamObservers has read access, for the chronicler (§8.7).
	TeamObservers = "observers"
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// GroupTeam is the team that owns a group's repositories.
func GroupTeam(group string) string { return "group-" + group }

// teams returns the teams of a workspace: the built-in ones, one per group,
// and the spec's extra teams. With groups, built-in teams don't get every
// repository; grouped repositories go to their group's team (and mergers).
func teams(w *v1alpha1.Workspace) []forgejo.Team {
	allRepos := len(w.Spec.Groups) == 0
	out := []forgejo.Team{
		{Name: TeamImplementers, Description: "Agents that write code", Permission: "write", IncludesAllRepositories: allRepos},
		{Name: TeamReviewers, Description: "Agents that review pull requests", Permission: "write", IncludesAllRepositories: allRepos},
		{Name: TeamMergers, Description: "May merge into protected branches", Permission: "write", IncludesAllRepositories: allRepos},
		{Name: TeamCI, Description: "CI and infrastructure", Permission: "write", IncludesAllRepositories: allRepos},
		{Name: TeamObservers, Description: "Read-only, for reporting", Permission: "read", IncludesAllRepositories: allRepos},
	}
	for _, g := range w.Spec.Groups {
		out = append(out, forgejo.Team{Name: GroupTeam(g.Name), Description: "Repositories of group " + g.Name, Permission: "write"})
	}
	for _, t := range w.Spec.Forge.Teams {
		perm := string(t.Permission)
		if perm == "" {
			perm = "write"
		}
		out = append(out, forgejo.Team{Name: t.Name, Permission: perm, IncludesAllRepositories: allRepos})
	}
	return out
}

func notReadyReason(err error, fallback string) string {
	if strings.Contains(err.Error(), deps.ErrNotReady.Error()) {
		return fallback
	}
	return "Error"
}

// reconcileForge provisions the org, teams, repositories and webhook. It
// sets ForgeOrgReady, RepositoriesReady and ForgeWebhookReady.
func (r *Reconciler) reconcileForge(ctx context.Context, w *v1alpha1.Workspace) error {
	org := w.Spec.Forge.Org
	fail := func(cond string, err error) error {
		setCond(w, cond, false, notReadyReason(err, "ForgeNotReady"), err.Error())
		return err
	}
	failAll := func(err error) error {
		_ = fail(ConditionRepositoriesReady, err)
		_ = fail(ConditionForgeWebhookReady, err)
		return fail(v1alpha1.ConditionForgeOrgReady, err)
	}
	if r.Deps.Forge == nil {
		return failAll(fmt.Errorf("no forge client: %w", deps.ErrNotReady))
	}
	fc, err := r.Deps.Forge(ctx)
	if err != nil {
		return failAll(err)
	}
	full := w.Spec.DisplayName
	if full == "" {
		full = w.Name
	}
	if _, err := fc.EnsureOrg(ctx, org, full); err != nil {
		return failAll(fmt.Errorf("org %s: %w", org, err))
	}
	ids := map[string]int64{}
	for _, t := range teams(w) {
		got, err := fc.EnsureTeam(ctx, org, t)
		if err != nil {
			return fail(v1alpha1.ConditionForgeOrgReady, fmt.Errorf("team %s: %w", t.Name, err))
		}
		ids[t.Name] = got.ID
	}
	for _, t := range w.Spec.Forge.Teams {
		for _, m := range t.Members {
			// Agents' users appear when their room is created; the room
			// controller adds its teams then. Skip users not there yet.
			if _, err := fc.GetUser(ctx, m); forgejo.IsNotFound(err) {
				continue
			} else if err != nil {
				return fail(v1alpha1.ConditionForgeOrgReady, err)
			}
			if err := fc.AddTeamMember(ctx, ids[t.Name], m); err != nil {
				return fail(v1alpha1.ConditionForgeOrgReady, fmt.Errorf("team %s member %s: %w", t.Name, m, err))
			}
		}
	}
	setCond(w, v1alpha1.ConditionForgeOrgReady, true, "Ready", fmt.Sprintf("org %s with %d teams", org, len(ids)))

	others, oerr := r.protectOtherRepos(ctx, w, fc)
	if err := errors.Join(r.reconcileRepos(ctx, w, fc, ids), oerr); err != nil {
		_ = fail(ConditionRepositoriesReady, err)
		// Carry on with the webhook; the repositories retry.
	} else {
		setCond(w, ConditionRepositoriesReady, true, "Ready", fmt.Sprintf("%d repositories protected", len(w.Spec.Repositories)+others))
	}

	secret, created, err := r.ensureWebhookSecret(ctx, w)
	if err != nil {
		return fail(ConditionForgeWebhookReady, err)
	}
	// Forgejo never returns a hook's secret, so push it when we just made a new one.
	if _, err := fc.EnsureOrgHook(ctx, org, WebhookURL(w.Name), secret, forgejo.ForgeNotificationEvents, created); err != nil {
		return fail(ConditionForgeWebhookReady, fmt.Errorf("webhook: %w", err))
	}
	setCond(w, ConditionForgeWebhookReady, true, "Ready", "org webhook to "+WebhookURL(w.Name))
	return nil
}

func (r *Reconciler) reconcileRepos(ctx context.Context, w *v1alpha1.Workspace, fc *forgejo.Client, ids map[string]int64) error {
	approvals := int64(w.Spec.Forge.RequiredApprovals)
	if approvals == 0 {
		approvals = 1
	}
	signed := w.Spec.Forge.SignedCommits == nil || *w.Spec.Forge.SignedCommits
	var errs []string
	for _, repo := range w.Spec.Repositories {
		if err := r.reconcileRepo(ctx, w, fc, ids, repo, approvals, signed); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", repo.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// protectOtherRepos protects the default branch and v* tags of the org's
// repositories that aren't in spec.repositories: ones people (or Forgejo
// admins) created in Forgejo itself. Agents can't create repositories
// (§9.1). It returns how many it covered.
//
// This runs on the workspace's periodic resync (resyncEvery) rather than
// from a system webhook (forgejo.EnsureSystemHook). A webhook would need a
// new path from Forgejo into the operator (a Service, TLS, a NetworkPolicy)
// and its deliveries are lost while the operator is down or changing
// leader, so it would still need this resync to be reliable. The resync
// alone protects every repository within one period, whatever happened in
// between. Archived repositories and pull mirrors are left alone.
func (r *Reconciler) protectOtherRepos(ctx context.Context, w *v1alpha1.Workspace, fc *forgejo.Client) (int, error) {
	org := w.Spec.Forge.Org
	repos, err := fc.ListOrgRepos(ctx, org)
	if err != nil {
		return 0, fmt.Errorf("list repositories: %w", err)
	}
	approvals := int64(w.Spec.Forge.RequiredApprovals)
	if approvals == 0 {
		approvals = 1
	}
	signed := w.Spec.Forge.SignedCommits == nil || *w.Spec.Forge.SignedCommits
	var errs []string
	n := 0
	for _, repo := range repos {
		managed := slices.ContainsFunc(w.Spec.Repositories, func(s v1alpha1.RepositorySpec) bool { return strings.EqualFold(s.Name, repo.Name) })
		if managed || repo.Archived || repo.Mirror {
			continue
		}
		branch := repo.DefaultBranch
		if branch == "" {
			branch = "main"
		}
		if err := fc.EnsureBaseBranchProtection(ctx, org, repo.Name, forgejo.DefaultBranchProtection(branch, approvals, signed, nil)); err != nil {
			errs = append(errs, fmt.Sprintf("%s: protection: %v", repo.Name, err))
			continue
		}
		if err := fc.EnsureTagProtection(ctx, org, repo.Name, "v*", []string{TeamMergers}); err != nil {
			errs = append(errs, fmt.Sprintf("%s: tag protection: %v", repo.Name, err))
			continue
		}
		n++
	}
	if len(errs) > 0 {
		return n, errors.New(strings.Join(errs, "; "))
	}
	return n, nil
}

func (r *Reconciler) reconcileRepo(ctx context.Context, w *v1alpha1.Workspace, fc *forgejo.Client, ids map[string]int64, repo v1alpha1.RepositorySpec, approvals int64, signed bool) error {
	org := w.Spec.Forge.Org
	var got *forgejo.Repository
	var err error
	if repo.ImportURL != "" {
		opt := forgejo.MigrateOption{CloneAddr: repo.ImportURL, RepoName: repo.Name, RepoOwner: org, Private: true}
		if repo.ImportGitHub {
			// The whole history (ADR 0012): authors stay GitHub accounts
			// until linkGitHubAccounts attributes them to rooms.
			opt.Service = "github"
			opt.Issues, opt.PullRequests, opt.Labels, opt.Milestones, opt.Releases, opt.Wiki = true, true, true, true, true, true
		}
		if repo.ImportSecretName != "" {
			if _, err := fc.GetRepo(ctx, org, repo.Name); forgejo.IsNotFound(err) {
				// Read the credential only when there is something to import.
				user, token, err := r.repoCredential(ctx, w, repo.ImportSecretName)
				if err != nil {
					return fmt.Errorf("import secret: %w", err)
				}
				opt.AuthUsername, opt.AuthToken = user, token
			}
		}
		got, err = fc.EnsureMigratedRepo(ctx, opt)
		if err == nil && repo.ImportGitHub && repo.ImportSecretName != "" {
			// The GitHub token is for this one import: don't keep it.
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.WorkspaceNamespace(w.Name), Name: repo.ImportSecretName}}
			if derr := r.Delete(ctx, sec); client.IgnoreNotFound(derr) != nil {
				return fmt.Errorf("deleting the import credential: %w", derr)
			}
		}
	} else {
		got, err = fc.EnsureOrgRepo(ctx, org, repo.Name)
	}
	if err != nil {
		return err
	}
	var repoTeams []string
	switch {
	case repo.Group != "":
		repoTeams = []string{GroupTeam(repo.Group), TeamMergers}
	case len(w.Spec.Groups) > 0:
		for _, t := range teams(w) {
			if !strings.HasPrefix(t.Name, "group-") {
				repoTeams = append(repoTeams, t.Name)
			}
		}
	}
	for _, t := range repoTeams {
		if err := fc.AddTeamRepo(ctx, ids[t], org, repo.Name); err != nil {
			return fmt.Errorf("team %s: %w", t, err)
		}
	}
	branch := got.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	if err := fc.EnsureBranchProtection(ctx, org, repo.Name, forgejo.DefaultBranchProtection(branch, approvals, signed, repo.StatusChecks)); err != nil {
		return fmt.Errorf("protection: %w", err)
	}
	if err := fc.EnsureTagProtection(ctx, org, repo.Name, "v*", []string{TeamMergers}); err != nil {
		return fmt.Errorf("tag protection: %w", err)
	}
	if repo.PushMirror != "" {
		var user, token string
		if repo.MirrorSecretName != "" {
			if user, token, err = r.repoCredential(ctx, w, repo.MirrorSecretName); err != nil {
				return fmt.Errorf("mirror secret: %w", err)
			}
		}
		if err := fc.EnsurePushMirror(ctx, org, repo.Name, repo.PushMirror, user, token); err != nil {
			return fmt.Errorf("push mirror: %w", err)
		}
	}
	return nil
}

// repoCredential reads a git credential for imports and push mirrors from a
// Secret in the workspace namespace: key token, and username (default
// "oauth2", which GitHub accepts with a token).
func (r *Reconciler) repoCredential(ctx context.Context, w *v1alpha1.Workspace, name string) (user, token string, err error) {
	var sec corev1.Secret
	key := types.NamespacedName{Namespace: v1alpha1.WorkspaceNamespace(w.Name), Name: name}
	if err := r.Get(ctx, key, &sec); err != nil {
		return "", "", fmt.Errorf("%s: %w", key, err)
	}
	token = string(sec.Data["token"])
	if token == "" {
		return "", "", fmt.Errorf("%s has no key token", key)
	}
	user = string(sec.Data["username"])
	if user == "" {
		user = "oauth2"
	}
	return user, token, nil
}

// reconcileGateway keeps the workspace's LiteLLM team; its budget is the
// per-room budget times the number of rooms.
func (r *Reconciler) reconcileGateway(ctx context.Context, w *v1alpha1.Workspace, ns string) error {
	fail := func(err error) error {
		setCond(w, ConditionGatewayTeamReady, false, notReadyReason(err, "GatewayNotReady"), err.Error())
		return err
	}
	if r.Deps.Gateway == nil {
		return fail(fmt.Errorf("no gateway client: %w", deps.ErrNotReady))
	}
	gc, err := r.Deps.Gateway(ctx)
	if err != nil {
		return fail(err)
	}
	var rooms v1alpha1.RoomList
	if err := r.List(ctx, &rooms, client.InNamespace(ns)); err != nil {
		return err
	}
	team := gateway.Team{TeamID: gateway.TeamID(w.Name), TeamAlias: w.Name, BudgetDuration: "30d", Metadata: map[string]string{"workspace": w.Name}}
	msg := "team " + team.TeamID + ", no budget"
	if per := w.Spec.Models.Budgets.PerRoomMonthlyUSD; per > 0 {
		budget := float64(per) * float64(len(rooms.Items))
		team.MaxBudget = &budget
		msg = fmt.Sprintf("team %s, $%.0f per 30 days for %d rooms", team.TeamID, budget, len(rooms.Items))
	}
	if err := gc.EnsureTeam(ctx, team); err != nil {
		return fail(err)
	}
	setCond(w, ConditionGatewayTeamReady, true, "Ready", msg)
	return nil
}
