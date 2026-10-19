// Package forgejo is a small typed client for the Forgejo (>= 16) admin and
// org APIs the operator uses (docs/design.md §9). Field names follow
// Forgejo's swagger.v1.json.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Client talks to one Forgejo instance with an admin token.
type Client struct {
	// BaseURL is the instance root, e.g. http://forgejo.swarmageddon-platform.svc:3000.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client for the instance at baseURL using an admin token.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is a non-2xx answer from Forgejo.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("forgejo: %s %s: %d %s", e.Method, e.Path, e.Status, strings.TrimSpace(e.Body))
}

// IsNotFound reports whether err is a 404 from Forgejo.
func IsNotFound(err error) bool { return statusIs(err, http.StatusNotFound) }

// IsConflict reports whether err is a 409 or 422 (Forgejo uses both for "already exists").
func IsConflict(err error) bool {
	return statusIs(err, http.StatusConflict) || statusIs(err, http.StatusUnprocessableEntity)
}

func statusIs(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == code
}

// call options
type opt func(*http.Request)

// Sudo makes the request act as another user (admin tokens only).
func Sudo(user string) opt { return func(r *http.Request) { r.Header.Set("Sudo", user) } }

// do sends a JSON request to /api/v1+path and decodes a JSON answer into out (if not nil).
func (c *Client) do(ctx context.Context, method, path string, in, out any, opts ...opt) error {
	raw, err := c.raw(ctx, method, path, in, opts...)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("forgejo: %s %s: decode: %w", method, path, err)
	}
	return nil
}

func (c *Client) raw(ctx context.Context, method, path string, in any, opts ...opt) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/v1"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	for _, o := range opts {
		o(req)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	return raw, nil
}

func esc(s string) string { return url.PathEscape(s) }

// ---- users ----

// User is Forgejo's User (the fields we use).
type User struct {
	ID            int64  `json:"id"`
	Login         string `json:"login"`
	Email         string `json:"email"`
	FullName      string `json:"full_name"`
	Active        bool   `json:"active"`
	ProhibitLogin bool   `json:"prohibit_login"`
}

// CreateUserOption is the body of POST /admin/users.
type CreateUserOption struct {
	Username           string `json:"username"`
	Email              string `json:"email,omitempty"`
	FullName           string `json:"full_name,omitempty"`
	Password           string `json:"password,omitempty"`
	MustChangePassword *bool  `json:"must_change_password,omitempty"`
	SendNotify         bool   `json:"send_notify"`
	Visibility         string `json:"visibility,omitempty"`
}

// EditUserOption is the body of PATCH /admin/users/{u}; nil fields are left alone.
type EditUserOption struct {
	Active                  *bool   `json:"active,omitempty"`
	AllowCreateOrganization *bool   `json:"allow_create_organization,omitempty"`
	MaxRepoCreation         *int    `json:"max_repo_creation,omitempty"`
	ProhibitLogin           *bool   `json:"prohibit_login,omitempty"`
	MustChangePassword      *bool   `json:"must_change_password,omitempty"`
	Email                   *string `json:"email,omitempty"`
	FullName                *string `json:"full_name,omitempty"`
}

// GetUser returns a user by name.
func (c *Client) GetUser(ctx context.Context, name string) (*User, error) {
	var u User
	return &u, c.do(ctx, http.MethodGet, "/users/"+esc(name), nil, &u)
}

// CreateUser creates a user (admin).
func (c *Client) CreateUser(ctx context.Context, o CreateUserOption) (*User, error) {
	var u User
	return &u, c.do(ctx, http.MethodPost, "/admin/users", o, &u)
}

// EditUser changes a user (admin).
func (c *Client) EditUser(ctx context.Context, name string, o EditUserOption) (*User, error) {
	var u User
	return &u, c.do(ctx, http.MethodPatch, "/admin/users/"+esc(name), o, &u)
}

// EnsureAgentUser creates the agent's account if missing and applies the
// agent restrictions (§9.1): no personal repos, no orgs, active, may log in
// (tokens need it). password is used only on creation.
func (c *Client) EnsureAgentUser(ctx context.Context, name, email, fullName, password string) (*User, error) {
	if _, err := c.GetUser(ctx, name); IsNotFound(err) {
		f := false
		if _, err := c.CreateUser(ctx, CreateUserOption{
			Username: name, Email: email, FullName: fullName, Password: password,
			MustChangePassword: &f, Visibility: "private",
		}); err != nil && !IsConflict(err) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	t, f, zero := true, false, 0
	return c.EditUser(ctx, name, EditUserOption{
		Active: &t, ProhibitLogin: &f, MaxRepoCreation: &zero, AllowCreateOrganization: &f, MustChangePassword: &f,
	})
}

// DisableUser deactivates a user and blocks sign-in; history keeps the author.
func (c *Client) DisableUser(ctx context.Context, name string) error {
	t, f := true, false
	_, err := c.EditUser(ctx, name, EditUserOption{Active: &f, ProhibitLogin: &t})
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ---- SSH keys ----

// PublicKey is an SSH key.
type PublicKey struct {
	ID          int64  `json:"id"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	Fingerprint string `json:"fingerprint"`
}

// ListUserKeys lists a user's SSH keys.
func (c *Client) ListUserKeys(ctx context.Context, user string) ([]PublicKey, error) {
	var ks []PublicKey
	return ks, c.do(ctx, http.MethodGet, "/users/"+esc(user)+"/keys?limit=50", nil, &ks)
}

// AddUserKey adds an SSH key to a user (admin).
func (c *Client) AddUserKey(ctx context.Context, user, title, key string) (*PublicKey, error) {
	var k PublicKey
	return &k, c.do(ctx, http.MethodPost, "/admin/users/"+esc(user)+"/keys",
		map[string]any{"title": title, "key": key, "read_only": false}, &k)
}

// DeleteUserKey removes an SSH key (admin).
func (c *Client) DeleteUserKey(ctx context.Context, user string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/admin/users/%s/keys/%d", esc(user), id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// EnsureUserKey makes the user's SSH keys exactly [key] under title: it adds
// key if missing and deletes keys with the same title that differ (rotation).
func (c *Client) EnsureUserKey(ctx context.Context, user, title, key string) error {
	ks, err := c.ListUserKeys(ctx, user)
	if err != nil {
		return err
	}
	want := keyBody(key)
	found := false
	for _, k := range ks {
		if keyBody(k.Key) == want {
			found = true
			continue
		}
		if k.Title == title {
			if err := c.DeleteUserKey(ctx, user, k.ID); err != nil {
				return err
			}
		}
	}
	if found {
		return nil
	}
	_, err = c.AddUserKey(ctx, user, title, key)
	return err
}

// keyBody is "type base64" without the comment.
func keyBody(k string) string {
	f := strings.Fields(k)
	if len(f) >= 2 {
		return f[0] + " " + f[1]
	}
	return strings.TrimSpace(k)
}

// ---- tokens ----

// AccessToken is an API token; SHA1 is the secret, returned only on creation.
type AccessToken struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	SHA1           string   `json:"sha1"`
	TokenLastEight string   `json:"token_last_eight"`
	Scopes         []string `json:"scopes"`
}

// AgentTokenScopes are the scopes of an agent's token (§9.1).
var AgentTokenScopes = []string{
	"write:repository", "write:issue", "read:organization", "read:user", "write:package", "read:notification",
}

// CreateUserToken issues a token for a user (Forgejo >= 16, admin).
func (c *Client) CreateUserToken(ctx context.Context, user, name string, scopes []string) (*AccessToken, error) {
	var t AccessToken
	return &t, c.do(ctx, http.MethodPost, "/admin/users/"+esc(user)+"/tokens",
		map[string]any{"name": name, "scopes": scopes}, &t)
}

// ListUserTokens lists a user's tokens (admin).
func (c *Client) ListUserTokens(ctx context.Context, user string) ([]AccessToken, error) {
	var ts []AccessToken
	return ts, c.do(ctx, http.MethodGet, "/admin/users/"+esc(user)+"/tokens?limit=50", nil, &ts)
}

// DeleteUserToken deletes a user's token by name or ID (admin).
func (c *Client) DeleteUserToken(ctx context.Context, user, token string) error {
	err := c.do(ctx, http.MethodDelete, "/admin/users/"+esc(user)+"/tokens/"+esc(token), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteTokensExcept deletes the user's tokens whose name starts with prefix,
// except keep (by name).
func (c *Client) DeleteTokensExcept(ctx context.Context, user, prefix, keep string) error {
	ts, err := c.ListUserTokens(ctx, user)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if strings.HasPrefix(t.Name, prefix) && t.Name != keep {
			if err := c.DeleteUserToken(ctx, user, fmt.Sprint(t.ID)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- GPG keys (via Sudo, §9.2) ----

// GPGKey is a user's GPG key.
type GPGKey struct {
	ID       int64  `json:"id"`
	KeyID    string `json:"key_id"`
	Verified bool   `json:"verified"`
}

// GPGKeyToken returns the token to sign to prove ownership of a key, as user.
func (c *Client) GPGKeyToken(ctx context.Context, user string) (string, error) {
	raw, err := c.raw(ctx, http.MethodGet, "/user/gpg_key_token", nil, Sudo(user))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(raw))
	var js string // Forgejo answers plain text; accept a JSON string too.
	if json.Unmarshal(raw, &js) == nil {
		s = js
	}
	return s, nil
}

// ListGPGKeys lists a user's GPG keys.
func (c *Client) ListGPGKeys(ctx context.Context, user string) ([]GPGKey, error) {
	var ks []GPGKey
	return ks, c.do(ctx, http.MethodGet, "/users/"+esc(user)+"/gpg_keys?limit=50", nil, &ks)
}

// AddGPGKey adds an armored public key as user; with a signature of the
// GPGKeyToken the key is verified at once.
func (c *Client) AddGPGKey(ctx context.Context, user, armoredPublic, armoredSignature string) (*GPGKey, error) {
	var k GPGKey
	return &k, c.do(ctx, http.MethodPost, "/user/gpg_keys",
		map[string]any{"armored_public_key": armoredPublic, "armored_signature": armoredSignature}, &k, Sudo(user))
}

// VerifyGPGKey verifies an already added key, as user.
func (c *Client) VerifyGPGKey(ctx context.Context, user, keyID, armoredSignature string) (*GPGKey, error) {
	var k GPGKey
	return &k, c.do(ctx, http.MethodPost, "/user/gpg_key_verify",
		map[string]any{"key_id": keyID, "armored_signature": armoredSignature}, &k, Sudo(user))
}

// DeleteGPGKey deletes one of user's GPG keys, as user.
func (c *Client) DeleteGPGKey(ctx context.Context, user string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/user/gpg_keys/%d", id), nil, nil, Sudo(user))
	if IsNotFound(err) {
		return nil
	}
	return err
}

// EnsureGPGKey makes keyID the user's only GPG key, verified. sign signs the
// verification token and returns an armored detached signature.
func (c *Client) EnsureGPGKey(ctx context.Context, user, keyID, armoredPublic string, sign func(token string) (string, error)) error {
	ks, err := c.ListGPGKeys(ctx, user)
	if err != nil {
		return err
	}
	have := false
	for _, k := range ks {
		if strings.EqualFold(k.KeyID, keyID) {
			if k.Verified {
				have = true
				continue
			}
		}
		if err := c.DeleteGPGKey(ctx, user, k.ID); err != nil {
			return err
		}
	}
	if have {
		return nil
	}
	tok, err := c.GPGKeyToken(ctx, user)
	if err != nil {
		return err
	}
	sig, err := sign(tok)
	if err != nil {
		return err
	}
	_, err = c.AddGPGKey(ctx, user, armoredPublic, sig)
	return err
}

// ---- orgs and teams ----

// Organization is a Forgejo org.
type Organization struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Username   string `json:"username"`
	Visibility string `json:"visibility"`
}

// GetOrg returns an org.
func (c *Client) GetOrg(ctx context.Context, org string) (*Organization, error) {
	var o Organization
	return &o, c.do(ctx, http.MethodGet, "/orgs/"+esc(org), nil, &o)
}

// EnsureOrg creates a private org if it doesn't exist.
func (c *Client) EnsureOrg(ctx context.Context, org, fullName string) (*Organization, error) {
	o, err := c.GetOrg(ctx, org)
	if err == nil || !IsNotFound(err) {
		return o, err
	}
	var out Organization
	err = c.do(ctx, http.MethodPost, "/orgs", map[string]any{
		"username": org, "full_name": fullName, "visibility": "private", "repo_admin_change_team_access": false,
	}, &out)
	return &out, err
}

// Team is an org team.
type Team struct {
	ID                      int64             `json:"id"`
	Name                    string            `json:"name"`
	Description             string            `json:"description,omitempty"`
	Permission              string            `json:"permission,omitempty"`
	CanCreateOrgRepo        bool              `json:"can_create_org_repo"`
	IncludesAllRepositories bool              `json:"includes_all_repositories"`
	UnitsMap                map[string]string `json:"units_map,omitempty"`
}

// RepoUnits are the unit keys teams get access to.
var RepoUnits = []string{
	"repo.code", "repo.issues", "repo.ext_issues", "repo.wiki", "repo.ext_wiki",
	"repo.pulls", "repo.releases", "repo.projects", "repo.packages", "repo.actions",
}

// UnitsFor grants permission on every unit.
func UnitsFor(permission string) map[string]string {
	m := make(map[string]string, len(RepoUnits))
	for _, u := range RepoUnits {
		m[u] = permission
	}
	return m
}

// ListTeams lists an org's teams.
func (c *Client) ListTeams(ctx context.Context, org string) ([]Team, error) {
	var ts []Team
	return ts, c.do(ctx, http.MethodGet, "/orgs/"+esc(org)+"/teams?limit=50", nil, &ts)
}

// EnsureTeam creates or updates a team so that it matches want. Teams never
// may create org repos (§9.1).
func (c *Client) EnsureTeam(ctx context.Context, org string, want Team) (*Team, error) {
	want.CanCreateOrgRepo = false
	if want.Permission == "" {
		want.Permission = "write"
	}
	if want.UnitsMap == nil {
		want.UnitsMap = UnitsFor(want.Permission)
	}
	ts, err := c.ListTeams(ctx, org)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"name": want.Name, "description": want.Description, "permission": want.Permission,
		"can_create_org_repo": false, "includes_all_repositories": want.IncludesAllRepositories,
		"units_map": want.UnitsMap,
	}
	for _, t := range ts {
		if t.Name != want.Name {
			continue
		}
		if t.Permission == want.Permission && !t.CanCreateOrgRepo &&
			t.IncludesAllRepositories == want.IncludesAllRepositories && t.Description == want.Description {
			return &t, nil
		}
		var out Team
		return &out, c.do(ctx, http.MethodPatch, fmt.Sprintf("/teams/%d", t.ID), body, &out)
	}
	var out Team
	return &out, c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/teams", body, &out)
}

// FindTeam returns the team named name in org, or nil.
func (c *Client) FindTeam(ctx context.Context, org, name string) (*Team, error) {
	ts, err := c.ListTeams(ctx, org)
	if err != nil {
		return nil, err
	}
	for i := range ts {
		if ts[i].Name == name {
			return &ts[i], nil
		}
	}
	return nil, nil
}

// AddTeamMember adds a user to a team (idempotent).
func (c *Client) AddTeamMember(ctx context.Context, teamID int64, user string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/teams/%d/members/%s", teamID, esc(user)), nil, nil)
}

// RemoveTeamMember removes a user from a team.
func (c *Client) RemoveTeamMember(ctx context.Context, teamID int64, user string) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/teams/%d/members/%s", teamID, esc(user)), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ListTeamRepos lists the repositories a team has access to (all pages).
func (c *Client) ListTeamRepos(ctx context.Context, teamID int64) ([]Repository, error) {
	var all []Repository
	for page := 1; ; page++ {
		var rs []Repository
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/teams/%d/repos?limit=50&page=%d", teamID, page), nil, &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 50 {
			return all, nil
		}
	}
}

// AddTeamRepo gives a team access to an org repo.
func (c *Client) AddTeamRepo(ctx context.Context, teamID int64, org, repo string) error {
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/teams/%d/repos/%s/%s", teamID, esc(org), esc(repo)), nil, nil)
}

// ---- repositories ----

// Repository is a repo (the fields we use).
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Empty         bool   `json:"empty"`
	Mirror        bool   `json:"mirror"`
	Archived      bool   `json:"archived"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
}

// GetRepo returns a repo.
func (c *Client) GetRepo(ctx context.Context, owner, repo string) (*Repository, error) {
	var r Repository
	return &r, c.do(ctx, http.MethodGet, "/repos/"+esc(owner)+"/"+esc(repo), nil, &r)
}

// ListOrgRepos lists an org's repos (first 50 per page, all pages).
func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]Repository, error) {
	var all []Repository
	for page := 1; ; page++ {
		var rs []Repository
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/orgs/%s/repos?limit=50&page=%d", esc(org), page), nil, &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 50 {
			return all, nil
		}
	}
}

// EnsureOrgRepo creates a private repo in org with a main branch if missing.
func (c *Client) EnsureOrgRepo(ctx context.Context, org, name string) (*Repository, error) {
	r, err := c.GetRepo(ctx, org, name)
	if err == nil || !IsNotFound(err) {
		return r, err
	}
	var out Repository
	err = c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/repos", map[string]any{
		"name": name, "private": true, "auto_init": true, "default_branch": "main", "trust_model": "committer",
	}, &out)
	return &out, err
}

// RunCron runs one of Forgejo's cron tasks now (POST /admin/cron/{task}).
func (c *Client) RunCron(ctx context.Context, task string) error {
	return c.do(ctx, http.MethodPost, "/admin/cron/"+esc(task), nil, nil)
}

// MigrateOption is the body of POST /repos/migrate.
type MigrateOption struct {
	CloneAddr    string `json:"clone_addr"`
	RepoName     string `json:"repo_name"`
	RepoOwner    string `json:"repo_owner"`
	Private      bool   `json:"private"`
	AuthUsername string `json:"auth_username,omitempty"`
	AuthToken    string `json:"auth_token,omitempty"`
	Service      string `json:"service,omitempty"`
	// What else to bring, for a service such as github.
	Issues       bool `json:"issues,omitempty"`
	PullRequests bool `json:"pull_requests,omitempty"`
	Labels       bool `json:"labels,omitempty"`
	Milestones   bool `json:"milestones,omitempty"`
	Releases     bool `json:"releases,omitempty"`
	Wiki         bool `json:"wiki,omitempty"`
}

// EnsureMigratedRepo imports a repo from a URL if it doesn't exist yet.
// The credential is used for this one import only.
func (c *Client) EnsureMigratedRepo(ctx context.Context, o MigrateOption) (*Repository, error) {
	r, err := c.GetRepo(ctx, o.RepoOwner, o.RepoName)
	if err == nil || !IsNotFound(err) {
		return r, err
	}
	o.Private = true
	// Forgejo migrates within the request; with issues and pull requests
	// that can take many minutes.
	long := *c
	hc := *c.HTTP
	hc.Timeout = 0
	long.HTTP = &hc
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	var out Repository
	err = long.do(ctx, http.MethodPost, "/repos/migrate", o, &out)
	return &out, err
}

// ---- branch and tag protection (§9.3) ----

// BranchProtection is CreateBranchProtectionOption / BranchProtection.
type BranchProtection struct {
	RuleName                string   `json:"rule_name"`
	EnablePush              bool     `json:"enable_push"`
	RequireSignedCommits    bool     `json:"require_signed_commits"`
	RequiredApprovals       int64    `json:"required_approvals"`
	DismissStaleApprovals   bool     `json:"dismiss_stale_approvals"`
	BlockOnRejectedReviews  bool     `json:"block_on_rejected_reviews"`
	EnableMergeWhitelist    bool     `json:"enable_merge_whitelist"`
	MergeWhitelistTeams     []string `json:"merge_whitelist_teams"`
	EnableStatusCheck       bool     `json:"enable_status_check"`
	StatusCheckContexts     []string `json:"status_check_contexts"`
	ApplyToAdmins           bool     `json:"apply_to_admins"`
	ProtectedFilePatterns   string   `json:"protected_file_patterns,omitempty"`
	UnprotectedFilePatterns string   `json:"unprotected_file_patterns,omitempty"`
}

// DefaultBranchProtection is the rule of §9.3 for branch.
func DefaultBranchProtection(branch string, approvals int64, signed bool, statusChecks []string) BranchProtection {
	if statusChecks == nil {
		statusChecks = []string{}
	}
	return BranchProtection{
		RuleName: branch, EnablePush: false, RequireSignedCommits: signed, RequiredApprovals: approvals,
		DismissStaleApprovals: true, BlockOnRejectedReviews: true,
		EnableMergeWhitelist: true, MergeWhitelistTeams: []string{"mergers"},
		EnableStatusCheck: len(statusChecks) > 0, StatusCheckContexts: statusChecks, ApplyToAdmins: true,
	}
}

// EnsureBranchProtection creates the rule, or patches it if it exists.
func (c *Client) EnsureBranchProtection(ctx context.Context, owner, repo string, bp BranchProtection) error {
	base := "/repos/" + esc(owner) + "/" + esc(repo) + "/branch_protections"
	var cur BranchProtection
	err := c.do(ctx, http.MethodGet, base+"/"+esc(bp.RuleName), nil, &cur)
	if IsNotFound(err) {
		return c.do(ctx, http.MethodPost, base, bp, nil)
	}
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, base+"/"+esc(bp.RuleName), bp, nil)
}

// EnsureBaseBranchProtection creates bp if the branch has no rule. An
// existing rule is brought up to bp, keeping its status checks and file
// patterns (people edit those, §9.3) and a higher approval count; nothing is
// written when it already holds.
func (c *Client) EnsureBaseBranchProtection(ctx context.Context, owner, repo string, bp BranchProtection) error {
	base := "/repos/" + esc(owner) + "/" + esc(repo) + "/branch_protections"
	var cur BranchProtection
	err := c.do(ctx, http.MethodGet, base+"/"+esc(bp.RuleName), nil, &cur)
	if IsNotFound(err) {
		return c.do(ctx, http.MethodPost, base, bp, nil)
	}
	if err != nil {
		return err
	}
	bp.EnableStatusCheck, bp.StatusCheckContexts = cur.EnableStatusCheck, cur.StatusCheckContexts
	if bp.StatusCheckContexts == nil {
		bp.StatusCheckContexts = []string{}
	}
	bp.ProtectedFilePatterns, bp.UnprotectedFilePatterns = cur.ProtectedFilePatterns, cur.UnprotectedFilePatterns
	bp.RequiredApprovals = max(bp.RequiredApprovals, cur.RequiredApprovals)
	if cur.EnablePush == bp.EnablePush && cur.RequireSignedCommits == bp.RequireSignedCommits &&
		cur.RequiredApprovals == bp.RequiredApprovals && cur.DismissStaleApprovals == bp.DismissStaleApprovals &&
		cur.BlockOnRejectedReviews == bp.BlockOnRejectedReviews && cur.EnableMergeWhitelist == bp.EnableMergeWhitelist &&
		slices.Equal(cur.MergeWhitelistTeams, bp.MergeWhitelistTeams) && cur.ApplyToAdmins == bp.ApplyToAdmins {
		return nil
	}
	return c.do(ctx, http.MethodPatch, base+"/"+esc(bp.RuleName), bp, nil)
}

// TagProtection protects tags matching NamePattern.
type TagProtection struct {
	ID                 int64    `json:"id,omitempty"`
	NamePattern        string   `json:"name_pattern"`
	WhitelistTeams     []string `json:"whitelist_teams"`
	WhitelistUsernames []string `json:"whitelist_usernames"`
}

// EnsureTagProtection adds a tag protection for pattern if none exists.
func (c *Client) EnsureTagProtection(ctx context.Context, owner, repo, pattern string, teams []string) error {
	base := "/repos/" + esc(owner) + "/" + esc(repo) + "/tag_protections"
	var cur []TagProtection
	if err := c.do(ctx, http.MethodGet, base, nil, &cur); err != nil {
		return err
	}
	for _, t := range cur {
		if t.NamePattern == pattern {
			return nil
		}
	}
	if teams == nil {
		teams = []string{}
	}
	return c.do(ctx, http.MethodPost, base, TagProtection{NamePattern: pattern, WhitelistTeams: teams, WhitelistUsernames: []string{}}, nil)
}

// ---- push mirrors (§9.5) ----

// PushMirror is a repo's push mirror.
type PushMirror struct {
	RemoteName    string `json:"remote_name"`
	RemoteAddress string `json:"remote_address"`
	Interval      string `json:"interval"`
	LastError     string `json:"last_error"`
}

// EnsurePushMirror adds a push mirror to address if none points there.
func (c *Client) EnsurePushMirror(ctx context.Context, owner, repo, address, username, password string) error {
	base := "/repos/" + esc(owner) + "/" + esc(repo) + "/push_mirrors"
	var cur []PushMirror
	if err := c.do(ctx, http.MethodGet, base, nil, &cur); err != nil {
		return err
	}
	for _, m := range cur {
		if m.RemoteAddress == address {
			return nil
		}
	}
	return c.do(ctx, http.MethodPost, base, map[string]any{
		"remote_address": address, "remote_username": username, "remote_password": password,
		"interval": "8h0m0s", "sync_on_commit": true,
	}, nil)
}

// ---- system webhooks ----

// Hook is a system webhook.
type Hook struct {
	ID        int64             `json:"id"`
	Type      string            `json:"type"`
	Active    bool              `json:"active"`
	Events    []string          `json:"events"`
	Config    map[string]string `json:"config"`
	UpdatedAt string            `json:"updated_at"`
}

// EnsureSystemHook adds a system webhook (type forgejo, JSON) to url if missing.
func (c *Client) EnsureSystemHook(ctx context.Context, hookURL, secret string, events []string) error {
	var cur []Hook
	if err := c.do(ctx, http.MethodGet, "/admin/hooks", nil, &cur); err != nil {
		return err
	}
	for _, h := range cur {
		if h.Config["url"] == hookURL {
			return nil
		}
	}
	cfg := map[string]string{"url": hookURL, "content_type": "json"}
	if secret != "" {
		cfg["secret"] = secret
	}
	return c.do(ctx, http.MethodPost, "/admin/hooks", map[string]any{
		"type": "forgejo", "active": true, "events": events, "config": cfg,
	}, nil)
}

// ---- Actions runners (§9.4) ----

// RunJob is a job waiting for (or assigned to) a runner.
type RunJob struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	RepoID int64    `json:"repo_id"`
	Name   string   `json:"name"`
	RunsOn []string `json:"runs_on"`
	Status string   `json:"status"`
	Handle string   `json:"handle"`
}

// WaitingJobs lists an org's jobs matching labels. Forgejo may answer null.
func (c *Client) WaitingJobs(ctx context.Context, org string, labels []string) ([]RunJob, error) {
	var js []RunJob
	q := url.Values{"labels": {strings.Join(labels, ",")}}
	if err := c.do(ctx, http.MethodGet, "/orgs/"+esc(org)+"/actions/runners/jobs?"+q.Encode(), nil, &js); err != nil {
		return nil, err
	}
	out := js[:0]
	for _, j := range js {
		if j.Status == "" || strings.Contains(j.Status, "waiting") {
			out = append(out, j)
		}
	}
	return out, nil
}

// RegisteredRunner is the answer to a runner registration.
type RegisteredRunner struct {
	ID    int64  `json:"id"`
	UUID  string `json:"uuid"`
	Token string `json:"token"`
}

// RegisterOrgRunner registers a runner on an org (Forgejo >= 15).
func (c *Client) RegisterOrgRunner(ctx context.Context, org, name, description string, ephemeral bool) (*RegisteredRunner, error) {
	var r RegisteredRunner
	return &r, c.do(ctx, http.MethodPost, "/orgs/"+esc(org)+"/actions/runners",
		map[string]any{"name": name, "description": description, "ephemeral": ephemeral}, &r)
}

// RegisterRepoRunner registers a runner on one repository (Forgejo >= 15):
// it only ever gets that repository's jobs.
func (c *Client) RegisterRepoRunner(ctx context.Context, owner, repo, name, description string, ephemeral bool) (*RegisteredRunner, error) {
	var r RegisteredRunner
	return &r, c.do(ctx, http.MethodPost, "/repos/"+esc(owner)+"/"+esc(repo)+"/actions/runners",
		map[string]any{"name": name, "description": description, "ephemeral": ephemeral}, &r)
}

// DeleteRepoRunner removes a repository's runner.
func (c *Client) DeleteRepoRunner(ctx context.Context, owner, repo string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/%s/actions/runners/%d", esc(owner), esc(repo), id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// DeleteOrgRunner removes a runner.
func (c *Client) DeleteOrgRunner(ctx context.Context, org string, id int64) error {
	err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/orgs/%s/actions/runners/%d", esc(org), id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
