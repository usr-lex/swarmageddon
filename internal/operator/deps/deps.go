// Package deps is what cmd/operator hands every controller's
// SetupWithManager(mgr, deps.Deps). It declares the small Forgejo and LiteLLM
// interfaces the workspace controller needs; internal/forgejo and
// internal/gateway provide the concrete clients.
package deps

import (
	"context"
	"errors"

	"k8s.io/utils/clock"

	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// Deps are the shared dependencies of the operator's controllers.
type Deps struct {
	Clock clock.Clock
	// PKI hands out the platform CA (Secret platform-ca), creating it on first use.
	PKI *pki.Store
	// Images the operator deploys, at this operator's version.
	Images images.Images
	// Version of the operator (internal/version.Version).
	Version string
	// ForgeFactory returns a Forgejo admin client. It fails with
	// ErrNotReady until Forgejo is deployed and its admin token exists.
	ForgeFactory func(ctx context.Context) (ForgeClient, error)
	// GatewayFactory returns a LiteLLM admin client. It fails with
	// ErrNotReady until LiteLLM is deployed and its master key exists.
	GatewayFactory func(ctx context.Context) (GatewayClient, error)
}

// ErrNotReady means a platform service isn't deployed or reachable yet;
// controllers set a condition and retry later.
var ErrNotReady = errors.New("platform service not ready")

// ForgeClient is the Forgejo admin API the workspace controller uses
// (docs/design.md §9). Every method is idempotent: it creates what is
// missing and updates what differs.
type ForgeClient interface {
	// EnsureOrg creates the organization if missing (private by default).
	EnsureOrg(ctx context.Context, org Org) error
	// EnsureTeam creates or updates a team in org. can_create_org_repo is
	// always false.
	EnsureTeam(ctx context.Context, org string, team Team) error
	// EnsureTeamMembers adds the given users to the team. Users that don't
	// exist yet are skipped without error (the room controller adds a
	// room's teams when it creates the user). It never removes members.
	EnsureTeamMembers(ctx context.Context, org, team string, users []string) error
	// EnsureTeamRepo gives a team access to one repository of org.
	EnsureTeamRepo(ctx context.Context, org, team, repo string) error
	// EnsureRepo creates the repository, or migrates it from r.ImportURL,
	// if missing. It returns the default branch.
	EnsureRepo(ctx context.Context, org string, r Repo) (defaultBranch string, err error)
	// EnsureBranchProtection creates or updates the rule named p.Branch.
	EnsureBranchProtection(ctx context.Context, org, repo string, p BranchProtection) error
	// EnsurePushMirror adds a push mirror to m.RemoteURL if there is none.
	EnsurePushMirror(ctx context.Context, org, repo string, m PushMirror) error
	// EnsureOrgWebhook creates the org-level webhook to h.URL, or updates
	// it (events, secret, active) if one to that URL exists (§9.7).
	EnsureOrgWebhook(ctx context.Context, org string, h Webhook) error
}

// Webhook is an org-level Forgejo webhook of type "forgejo".
type Webhook struct {
	URL string
	// ContentType is "json".
	ContentType string
	// Secret signs deliveries (HMAC-SHA256, X-Forgejo-Signature).
	Secret string
	// Events, as Forgejo names them (e.g. issues, issue_comment,
	// pull_request, pull_request_review_approved, workflow_run).
	Events []string
	Active bool
}

// Org is a Forgejo organization.
type Org struct {
	Name     string
	FullName string
	// Visibility: public, limited or private (default private).
	Visibility string
}

// Team is a Forgejo team.
type Team struct {
	Name        string
	Description string
	// Permission: read, write or admin.
	Permission string
	// IncludesAllRepositories gives the team every repository of the org.
	IncludesAllRepositories bool
	// Units maps unit names (repo.code, repo.pulls, ...) to none, read, write
	// or admin. Empty: every unit at Permission.
	Units map[string]string
}

// Repo is a repository to create or import.
type Repo struct {
	Name    string
	Private bool
	// ImportURL, if set, migrates the repository from there instead.
	ImportURL string
	// ImportToken authenticates the import (used once, never stored by us).
	ImportToken string
}

// BranchProtection is a branch protection rule (§9.3).
type BranchProtection struct {
	Branch               string
	RequiredApprovals    int
	RequireSignedCommits bool
	MergeWhitelistTeams  []string
	StatusCheckContexts  []string
	// ProtectedFilePatterns, e.g. ".forgejo/workflows/**", only admins of
	// the ci team may change (semicolon-separated in Forgejo).
	ProtectedFilePatterns string
}

// PushMirror mirrors a repository to an outside remote.
type PushMirror struct {
	RemoteURL string
	Username  string
	Password  string
	// Interval as a Go duration string, e.g. "8h0m0s".
	Interval     string
	SyncOnCommit bool
}

// GatewayClient is the LiteLLM admin API the workspace controller uses.
type GatewayClient interface {
	// EnsureTeam creates or updates the LiteLLM team with t.Alias and
	// returns its team_id. Room keys are created in this team.
	EnsureTeam(ctx context.Context, t GatewayTeam) (teamID string, err error)
}

// GatewayTeam is one workspace's team in LiteLLM.
type GatewayTeam struct {
	// Alias is the workspace name.
	Alias string
	// MaxBudgetUSD per BudgetDuration; 0 is unlimited.
	MaxBudgetUSD float64
	// BudgetDuration, e.g. "30d".
	BudgetDuration string
	// Models the team may use; empty: all.
	Models   []string
	Metadata map[string]string
}
