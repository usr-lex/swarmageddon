package forgejo

import (
	"context"
	"fmt"
	"net/http"
)

// ForgeNotificationEvents are the org webhook events behind forge
// notifications to agents (docs/design.md §9.7). Verified against Forgejo
// 17.0.0: "issues" and "pull_request" also enable their _assign, _label and
// _milestone events (and pull_request_sync); "pull_request_review" and
// "workflow_run" are not event names there and are dropped silently, so
// reviews and failed runs use the names below.
var ForgeNotificationEvents = []string{
	"issues", "issue_comment",
	"pull_request", "pull_request_comment", "pull_request_review_request",
	"pull_request_review_approved", "pull_request_review_rejected", "pull_request_review_comment",
	"action_run_failure",
}

// ListOrgHooks lists an org's webhooks.
func (c *Client) ListOrgHooks(ctx context.Context, org string) ([]Hook, error) {
	var hs []Hook
	return hs, c.do(ctx, http.MethodGet, "/orgs/"+esc(org)+"/hooks?limit=50", nil, &hs)
}

// EnsureOrgHook makes the org have one active forgejo-type JSON webhook to
// hookURL with these events and secret. Forgejo doesn't return the secret,
// so an existing hook is always patched.
func (c *Client) EnsureOrgHook(ctx context.Context, org, hookURL, secret string, events []string) (*Hook, error) {
	hs, err := c.ListOrgHooks(ctx, org)
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{"url": hookURL, "content_type": "json", "secret": secret}
	for _, h := range hs {
		if h.Config["url"] != hookURL {
			continue
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
// center uses to check repository access (§9.7).
var ReaderTokenScopes = []string{"read:admin", "read:repository", "read:organization", "read:user"}

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
