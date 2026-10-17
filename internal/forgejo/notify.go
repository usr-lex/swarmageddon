package forgejo

import (
	"context"
	"fmt"
	"net/http"
)

// ForgeNotificationEvents are the org webhook events behind forge
// notifications to agents (docs/design.md §9.7) and the observatory (§8.7):
// push for per-commit activity, workflow_run_success for passing CI.
// Verified against Forgejo 17.0.0; "workflow_run" is not an event there and
// is dropped silently.
var ForgeNotificationEvents = []string{
	"issues", "issue_comment", "pull_request", "pull_request_review",
	"pull_request_comment", "workflow_run_failure", "action_run_failure",
	"push", "workflow_run_success",
}

// eventExpansion is how Forgejo 17.0.0 stores the umbrella events it
// accepts (observed by creating hooks; the stored list is the expansion).
var eventExpansion = map[string][]string{
	"issues": {"issues", "issue_assign", "issue_label", "issue_milestone"},
	"pull_request": {"pull_request", "pull_request_assign", "pull_request_label", "pull_request_milestone",
		"pull_request_sync", "pull_request_review_request"},
	"pull_request_review": {"pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment"},
}

// ExpandEvents returns the set of event names Forgejo stores for events.
func ExpandEvents(events []string) map[string]bool {
	set := map[string]bool{}
	for _, e := range events {
		if x, ok := eventExpansion[e]; ok {
			for _, y := range x {
				set[y] = true
			}
			continue
		}
		set[e] = true
	}
	return set
}

func sameEvents(stored, want []string) bool {
	a, b := ExpandEvents(stored), ExpandEvents(want)
	if len(a) != len(b) {
		return false
	}
	for e := range b {
		if !a[e] {
			return false
		}
	}
	return true
}

// ListOrgHooks lists an org's webhooks.
func (c *Client) ListOrgHooks(ctx context.Context, org string) ([]Hook, error) {
	var hs []Hook
	return hs, c.do(ctx, http.MethodGet, "/orgs/"+esc(org)+"/hooks?limit=50", nil, &hs)
}

// EnsureOrgHook makes the org have one active forgejo-type JSON webhook to
// hookURL with these events and secret. An existing hook is patched only
// when its events, state or content type differ: Forgejo never returns the
// secret, so to change the secret pass rotateSecret.
func (c *Client) EnsureOrgHook(ctx context.Context, org, hookURL, secret string, events []string, rotateSecret bool) (*Hook, error) {
	hs, err := c.ListOrgHooks(ctx, org)
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{"url": hookURL, "content_type": "json", "secret": secret}
	for i, h := range hs {
		if h.Config["url"] != hookURL {
			continue
		}
		if !rotateSecret && h.Active && h.Config["content_type"] == "json" && sameEvents(h.Events, events) {
			return &hs[i], nil
		}
		var out Hook
		return &out, c.do(ctx, http.MethodPatch, fmt.Sprintf("/orgs/%s/hooks/%d", esc(org), h.ID),
			map[string]any{"active": true, "events": events, "config": cfg}, &out)
	}
	var out Hook
	return &out, c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/hooks",
		map[string]any{"type": "forgejo", "active": true, "events": events, "config": cfg}, &out)
}

// DeleteOrgHook deletes an org webhook.
func (c *Client) DeleteOrgHook(ctx context.Context, org string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/orgs/%s/hooks/%d", esc(org), id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ReaderTokenScopes are the scopes of the read-only token the control
// center uses to check repository access (§9.7) and to find agents' open
// issues and pull requests (§10.3).
var ReaderTokenScopes = []string{"read:admin", "read:repository", "read:organization", "read:user", "read:issue"}

// RepoPermission is a user's permission on a repository.
type RepoPermission struct {
	Permission string `json:"permission"` // none, read, write, admin, owner
	RoleName   string `json:"role_name"`
}

// CanRead reports whether the permission allows reading the repository.
func (p RepoPermission) CanRead() bool {
	switch p.Permission {
	case "read", "write", "admin", "owner":
		return true
	}
	return false
}

// GetRepoPermission returns user's permission on owner/repo.
func (c *Client) GetRepoPermission(ctx context.Context, owner, repo, user string) (*RepoPermission, error) {
	var p RepoPermission
	return &p, c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo)+"/collaborators/"+esc(user)+"/permission", nil, &p)
}

// DeleteOrg deletes an org (it must have no repositories).
func (c *Client) DeleteOrg(ctx context.Context, org string) error {
	err := c.do(ctx, http.MethodDelete, "/orgs/"+esc(org), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteRepo deletes a repository.
func (c *Client) DeleteRepo(ctx context.Context, owner, repo string) error {
	err := c.do(ctx, http.MethodDelete, "/repos/"+esc(owner)+"/"+esc(repo), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteUser deletes a user and purges what they own (tests and cleanup only;
// rooms are disabled, never deleted).
func (c *Client) DeleteUser(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/admin/users/"+esc(name)+"?purge=true", nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
