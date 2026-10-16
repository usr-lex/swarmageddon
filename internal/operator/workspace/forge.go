package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
)

// Built-in team names (design §9.1).
const (
	TeamImplementers = "implementers"
	TeamReviewers    = "reviewers"
	TeamMergers      = "mergers"
	TeamCI           = "ci"
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
// repository; grouped repositories go to their group's team only.
func teams(w *v1alpha1.Workspace) []deps.Team {
	allRepos := len(w.Spec.Groups) == 0
	out := []deps.Team{
		{Name: TeamImplementers, Description: "Agents that write code", Permission: "write", IncludesAllRepositories: allRepos},
		{Name: TeamReviewers, Description: "Agents that review pull requests", Permission: "write", IncludesAllRepositories: allRepos},
		{Name: TeamMergers, Description: "May merge into protected branches", Permission: "write", IncludesAllRepositories: allRepos},
		{Name: TeamCI, Description: "CI and infrastructure", Permission: "write", IncludesAllRepositories: allRepos},
	}
	for _, g := range w.Spec.Groups {
		out = append(out, deps.Team{Name: GroupTeam(g.Name), Description: "Repositories of group " + g.Name, Permission: "write"})
	}
	for _, t := range w.Spec.Forge.Teams {
		perm := string(t.Permission)
		if perm == "" {
			perm = "write"
		}
		out = append(out, deps.Team{Name: t.Name, Permission: perm, IncludesAllRepositories: allRepos})
	}
	return out
}

// reconcileForge provisions the org, teams, repositories and webhook. It
// sets ForgeOrgReady, RepositoriesReady and ForgeWebhookReady.
func (r *Reconciler) reconcileForge(ctx context.Context, w *v1alpha1.Workspace) error {
	org := w.Spec.Forge.Org
	fail := func(cond string, err error) error {
		reason := "Error"
		if strings.Contains(err.Error(), deps.ErrNotReady.Error()) {
			reason = "ForgeNotReady"
		}
		setCond(w, cond, false, reason, err.Error())
		return err
	}
	if r.Deps.ForgeFactory == nil {
		err := fmt.Errorf("no forge client: %w", deps.ErrNotReady)
		fail(ConditionRepositoriesReady, err)
		fail(ConditionForgeWebhookReady, err)
		return fail(v1alpha1.ConditionForgeOrgReady, err)
	}
	fc, err := r.Deps.ForgeFactory(ctx)
	if err != nil {
		fail(ConditionRepositoriesReady, err)
		fail(ConditionForgeWebhookReady, err)
		return fail(v1alpha1.ConditionForgeOrgReady, err)
	}
	full := w.Spec.DisplayName
	if full == "" {
		full = w.Name
	}
	if err := fc.EnsureOrg(ctx, deps.Org{Name: org, FullName: full, Visibility: "private"}); err != nil {
		return fail(v1alpha1.ConditionForgeOrgReady, fmt.Errorf("org %s: %w", org, err))
	}
	for _, t := range teams(w) {
		if err := fc.EnsureTeam(ctx, org, t); err != nil {
			return fail(v1alpha1.ConditionForgeOrgReady, fmt.Errorf("team %s: %w", t.Name, err))
		}
	}
	for _, t := range w.Spec.Forge.Teams {
		if len(t.Members) == 0 {
			continue
		}
		if err := fc.EnsureTeamMembers(ctx, org, t.Name, t.Members); err != nil {
			return fail(v1alpha1.ConditionForgeOrgReady, fmt.Errorf("team %s members: %w", t.Name, err))
		}
	}
	setCond(w, v1alpha1.ConditionForgeOrgReady, true, "Ready", fmt.Sprintf("org %s with %d teams", org, len(teams(w))))

	if err := r.reconcileRepos(ctx, w, fc); err != nil {
		fail(ConditionRepositoriesReady, err)
		// Carry on with the webhook; the repositories retry.
	} else {
		setCond(w, ConditionRepositoriesReady, true, "Ready", fmt.Sprintf("%d repositories protected", len(w.Spec.Repositories)))
	}

	secret, err := r.ensureWebhookSecret(ctx, w)
	if err != nil {
		return fail(ConditionForgeWebhookReady, err)
	}
	if err := fc.EnsureOrgWebhook(ctx, org, deps.Webhook{
		URL: WebhookURL(w.Name), ContentType: "json", Secret: secret, Events: WebhookEvents, Active: true,
	}); err != nil {
		return fail(ConditionForgeWebhookReady, fmt.Errorf("webhook: %w", err))
	}
	setCond(w, ConditionForgeWebhookReady, true, "Ready", "org webhook to "+WebhookURL(w.Name))
	return err
}

func (r *Reconciler) reconcileRepos(ctx context.Context, w *v1alpha1.Workspace, fc deps.ForgeClient) error {
	org := w.Spec.Forge.Org
	approvals := int(w.Spec.Forge.RequiredApprovals)
	if approvals == 0 {
		approvals = 1
	}
	signed := w.Spec.Forge.SignedCommits == nil || *w.Spec.Forge.SignedCommits
	var errs []string
	for _, repo := range w.Spec.Repositories {
		if err := r.reconcileRepo(ctx, w, fc, org, repo, approvals, signed); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", repo.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (r *Reconciler) reconcileRepo(ctx context.Context, w *v1alpha1.Workspace, fc deps.ForgeClient, org string, repo v1alpha1.RepositorySpec, approvals int, signed bool) error {
	branch, err := fc.EnsureRepo(ctx, org, deps.Repo{Name: repo.Name, Private: true, ImportURL: repo.ImportURL})
	if err != nil {
		return err
	}
	if repo.Group != "" {
		for _, team := range []string{GroupTeam(repo.Group), TeamMergers} {
			if err := fc.EnsureTeamRepo(ctx, org, team, repo.Name); err != nil {
				return fmt.Errorf("team %s: %w", team, err)
			}
		}
	} else if len(w.Spec.Groups) > 0 {
		for _, t := range teams(w) {
			if strings.HasPrefix(t.Name, "group-") {
				continue
			}
			if err := fc.EnsureTeamRepo(ctx, org, t.Name, repo.Name); err != nil {
				return fmt.Errorf("team %s: %w", t.Name, err)
			}
		}
	}
	if branch == "" {
		branch = "main"
	}
	if err := fc.EnsureBranchProtection(ctx, org, repo.Name, deps.BranchProtection{
		Branch:               branch,
		RequiredApprovals:    approvals,
		RequireSignedCommits: signed,
		MergeWhitelistTeams:  []string{TeamMergers},
		StatusCheckContexts:  repo.StatusChecks,
	}); err != nil {
		return fmt.Errorf("protection: %w", err)
	}
	if repo.PushMirror != "" {
		m := deps.PushMirror{RemoteURL: repo.PushMirror, Interval: "8h0m0s", SyncOnCommit: true}
		if repo.MirrorSecretName != "" {
			var sec corev1.Secret
			key := types.NamespacedName{Namespace: v1alpha1.SystemNamespace, Name: repo.MirrorSecretName}
			if err := r.Get(ctx, key, &sec); err != nil {
				return fmt.Errorf("mirror secret %s: %w", key, err)
			}
			m.Username, m.Password = "oauth2", string(sec.Data["token"])
		}
		if err := fc.EnsurePushMirror(ctx, org, repo.Name, m); err != nil {
			return fmt.Errorf("push mirror: %w", err)
		}
	}
	return nil
}

// reconcileGateway keeps the workspace's LiteLLM team; its budget is the
// per-room budget times the number of rooms.
func (r *Reconciler) reconcileGateway(ctx context.Context, w *v1alpha1.Workspace, ns string) error {
	if r.Deps.GatewayFactory == nil {
		err := fmt.Errorf("no gateway client: %w", deps.ErrNotReady)
		setCond(w, ConditionGatewayTeamReady, false, "GatewayNotReady", err.Error())
		return err
	}
	gc, err := r.Deps.GatewayFactory(ctx)
	if err != nil {
		setCond(w, ConditionGatewayTeamReady, false, "GatewayNotReady", err.Error())
		return err
	}
	var rooms v1alpha1.RoomList
	if err := r.List(ctx, &rooms, client.InNamespace(ns)); err != nil {
		return err
	}
	budget := float64(w.Spec.Models.Budgets.PerRoomMonthlyUSD) * float64(len(rooms.Items))
	id, err := gc.EnsureTeam(ctx, deps.GatewayTeam{
		Alias: w.Name, MaxBudgetUSD: budget, BudgetDuration: "30d",
		Metadata: map[string]string{"workspace": w.Name},
	})
	if err != nil {
		setCond(w, ConditionGatewayTeamReady, false, "Error", err.Error())
		return err
	}
	msg := fmt.Sprintf("team %s, unlimited", id)
	if budget > 0 {
		msg = fmt.Sprintf("team %s, $%.0f per 30 days for %d rooms", id, budget, len(rooms.Items))
	}
	setCond(w, ConditionGatewayTeamReady, true, "Ready", msg)
	return nil
}
