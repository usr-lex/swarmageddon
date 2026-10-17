package forgejo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type call struct {
	Method, Path, Sudo string
	Body               map[string]any
}

// fake answers by "METHOD path" (path includes the query) and records calls.
type fake struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []call
	routes map[string]func(w http.ResponseWriter)
}

func newFake(t *testing.T, routes map[string]func(w http.ResponseWriter)) (*fake, *Client) {
	f := &fake{t: t, routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token admintoken" {
			t.Errorf("missing auth on %s %s", r.Method, r.URL)
		}
		p := strings.TrimPrefix(r.URL.RequestURI(), "/api/v1")
		c := call{Method: r.Method, Path: p, Sudo: r.Header.Get("Sudo")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			if err := json.Unmarshal(b, &c.Body); err != nil {
				t.Errorf("body of %s %s: %v", r.Method, p, err)
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		if h, ok := f.routes[r.Method+" "+p]; ok {
			h(w)
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return f, New(srv.URL, "admintoken")
}

func jsonResp(code int, v any) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *fake) find(method, path string) *call {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.calls {
		if f.calls[i].Method == method && f.calls[i].Path == path {
			return &f.calls[i]
		}
	}
	return nil
}

func (f *fake) mustFind(method, path string) *call {
	f.t.Helper()
	c := f.find(method, path)
	if c == nil {
		f.t.Fatalf("no call %s %s; calls: %+v", method, path, f.calls)
	}
	return c
}

func TestEnsureAgentUserCreatesAndRestricts(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"PATCH /admin/users/athena": jsonResp(200, User{Login: "athena"}),
	})
	if _, err := c.EnsureAgentUser(context.Background(), "athena", "athena@agents.acme.invalid", "Athena", "pw"); err != nil {
		t.Fatal(err)
	}
	cr := f.mustFind("POST", "/admin/users")
	want := map[string]any{"username": "athena", "email": "athena@agents.acme.invalid", "full_name": "Athena",
		"password": "pw", "must_change_password": false, "send_notify": false, "visibility": "private"}
	if !reflect.DeepEqual(cr.Body, want) {
		t.Errorf("create body = %v", cr.Body)
	}
	p := f.mustFind("PATCH", "/admin/users/athena")
	if p.Body["max_repo_creation"] != float64(0) || p.Body["allow_create_organization"] != false || p.Body["active"] != true {
		t.Errorf("patch body = %v", p.Body)
	}
}

func TestEnsureAgentUserExistingSkipsCreate(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /users/athena":         jsonResp(200, User{Login: "athena"}),
		"PATCH /admin/users/athena": jsonResp(200, User{Login: "athena"}),
	})
	if _, err := c.EnsureAgentUser(context.Background(), "athena", "e", "A", "pw"); err != nil {
		t.Fatal(err)
	}
	if f.find("POST", "/admin/users") != nil {
		t.Error("created an existing user")
	}
}

func TestDisableUser(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"PATCH /admin/users/athena": jsonResp(200, User{}),
	})
	if err := c.DisableUser(context.Background(), "athena"); err != nil {
		t.Fatal(err)
	}
	b := f.mustFind("PATCH", "/admin/users/athena").Body
	if !reflect.DeepEqual(b, map[string]any{"active": false, "prohibit_login": true}) {
		t.Errorf("body = %v", b)
	}
}

func TestEnsureUserKeyRotates(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /users/athena/keys?limit=50": jsonResp(200, []PublicKey{{ID: 7, Key: "ssh-ed25519 OLD x", Title: "swarm"}, {ID: 8, Key: "ssh-rsa OTHER", Title: "mine"}}),
	})
	if err := c.EnsureUserKey(context.Background(), "athena", "swarm", "ssh-ed25519 NEW athena"); err != nil {
		t.Fatal(err)
	}
	f.mustFind("DELETE", "/admin/users/athena/keys/7")
	if f.find("DELETE", "/admin/users/athena/keys/8") != nil {
		t.Error("deleted a key with another title")
	}
	add := f.mustFind("POST", "/admin/users/athena/keys")
	if !reflect.DeepEqual(add.Body, map[string]any{"title": "swarm", "key": "ssh-ed25519 NEW athena", "read_only": false}) {
		t.Errorf("body = %v", add.Body)
	}
}

func TestCreateUserToken(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"POST /admin/users/athena/tokens": jsonResp(201, AccessToken{ID: 3, Name: "swarm-1", SHA1: "abc"}),
	})
	tok, err := c.CreateUserToken(context.Background(), "athena", "swarm-1", AgentTokenScopes)
	if err != nil || tok.SHA1 != "abc" {
		t.Fatal(tok, err)
	}
	b := f.mustFind("POST", "/admin/users/athena/tokens").Body
	if b["name"] != "swarm-1" || len(b["scopes"].([]any)) != 6 {
		t.Errorf("body = %v", b)
	}
}

func TestDeleteTokensExcept(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /admin/users/athena/tokens?limit=50": jsonResp(200, []AccessToken{{ID: 1, Name: "swarm-1"}, {ID: 2, Name: "swarm-2"}, {ID: 3, Name: "other"}}),
	})
	if err := c.DeleteTokensExcept(context.Background(), "athena", "swarm-", "swarm-2"); err != nil {
		t.Fatal(err)
	}
	f.mustFind("DELETE", "/admin/users/athena/tokens/1")
	if f.find("DELETE", "/admin/users/athena/tokens/2") != nil || f.find("DELETE", "/admin/users/athena/tokens/3") != nil {
		t.Error("deleted the wrong tokens")
	}
}

func TestEnsureGPGKeyUsesSudo(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /users/athena/gpg_keys?limit=50": jsonResp(200, []GPGKey{{ID: 4, KeyID: "OLD", Verified: true}}),
		"GET /user/gpg_key_token": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("tok123"))
		},
		"POST /user/gpg_keys": jsonResp(201, GPGKey{ID: 5, KeyID: "NEW", Verified: true}),
	})
	var signed string
	err := c.EnsureGPGKey(context.Background(), "athena", "NEW", "PUB", func(tok string) (string, error) {
		signed = tok
		return "SIG", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if signed != "tok123" {
		t.Errorf("signed %q", signed)
	}
	if d := f.mustFind("DELETE", "/user/gpg_keys/4"); d.Sudo != "athena" {
		t.Error("delete without sudo")
	}
	if tk := f.mustFind("GET", "/user/gpg_key_token"); tk.Sudo != "athena" {
		t.Error("token without sudo")
	}
	add := f.mustFind("POST", "/user/gpg_keys")
	if add.Sudo != "athena" || !reflect.DeepEqual(add.Body, map[string]any{"armored_public_key": "PUB", "armored_signature": "SIG"}) {
		t.Errorf("add = %+v", add)
	}
}

func TestEnsureOrgAndTeam(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /orgs/acme/teams?limit=50": jsonResp(200, []Team{{ID: 9, Name: "reviewers", Permission: "read"}}),
		"PATCH /teams/9":                jsonResp(200, Team{ID: 9, Name: "reviewers"}),
	})
	ctx := context.Background()
	if _, err := c.EnsureOrg(ctx, "acme", "Acme"); err != nil {
		t.Fatal(err)
	}
	if b := f.mustFind("POST", "/orgs").Body; b["username"] != "acme" || b["visibility"] != "private" {
		t.Errorf("org body = %v", b)
	}
	if _, err := c.EnsureTeam(ctx, "acme", Team{Name: "implementers", IncludesAllRepositories: true}); err != nil {
		t.Fatal(err)
	}
	tb := f.mustFind("POST", "/orgs/acme/teams").Body
	if tb["can_create_org_repo"] != false || tb["permission"] != "write" || tb["includes_all_repositories"] != true {
		t.Errorf("team body = %v", tb)
	}
	if um := tb["units_map"].(map[string]any); um["repo.code"] != "write" || len(um) != len(RepoUnits) {
		t.Errorf("units_map = %v", um)
	}
	if _, err := c.EnsureTeam(ctx, "acme", Team{Name: "reviewers", Permission: "write"}); err != nil {
		t.Fatal(err)
	}
	if b := f.mustFind("PATCH", "/teams/9").Body; b["permission"] != "write" || b["can_create_org_repo"] != false {
		t.Errorf("patch = %v", b)
	}
	if err := c.AddTeamMember(ctx, 9, "athena"); err != nil {
		t.Fatal(err)
	}
	f.mustFind("PUT", "/teams/9/members/athena")
}

// TestBaseBranchProtection: a repository the operator doesn't manage gets
// the rule; an existing rule is tightened but keeps people's status checks
// and a higher approval count; a rule that holds is left alone.
func TestBaseBranchProtection(t *testing.T) {
	weak := BranchProtection{RuleName: "main", EnablePush: true, RequiredApprovals: 3, EnableStatusCheck: true,
		StatusCheckContexts: []string{"ci/build"}, MergeWhitelistTeams: []string{"mergers"}}
	held := DefaultBranchProtection("main", 1, true, []string{"ci/x"})
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /repos/acme/weak/branch_protections/main": jsonResp(200, weak),
		"GET /repos/acme/held/branch_protections/main": jsonResp(200, held),
	})
	ctx := context.Background()
	bp := DefaultBranchProtection("main", 1, true, nil)
	for _, repo := range []string{"new", "weak", "held"} {
		if err := c.EnsureBaseBranchProtection(ctx, "acme", repo, bp); err != nil {
			t.Fatal(err)
		}
	}
	if b := f.mustFind("POST", "/repos/acme/new/branch_protections").Body; b["enable_push"] != false || b["require_signed_commits"] != true {
		t.Errorf("new rule = %v", b)
	}
	b := f.mustFind("PATCH", "/repos/acme/weak/branch_protections/main").Body
	if b["enable_push"] != false || b["apply_to_admins"] != true || b["required_approvals"] != float64(3) ||
		b["enable_status_check"] != true || !reflect.DeepEqual(b["status_check_contexts"], []any{"ci/build"}) {
		t.Errorf("tightened rule = %v", b)
	}
	if f.find("PATCH", "/repos/acme/held/branch_protections/main") != nil {
		t.Error("patched a rule that already holds")
	}
}

func TestReposAndProtection(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /repos/acme/web/branch_protections/main": jsonResp(200, BranchProtection{RuleName: "main"}),
		"GET /repos/acme/api/tag_protections":         jsonResp(200, []TagProtection{}),
		"GET /repos/acme/api/push_mirrors":            jsonResp(200, []PushMirror{}),
	})
	ctx := context.Background()
	if _, err := c.EnsureOrgRepo(ctx, "acme", "api"); err != nil {
		t.Fatal(err)
	}
	if b := f.mustFind("POST", "/orgs/acme/repos").Body; b["name"] != "api" || b["private"] != true || b["default_branch"] != "main" {
		t.Errorf("repo body = %v", b)
	}
	if _, err := c.EnsureMigratedRepo(ctx, MigrateOption{CloneAddr: "https://github.com/x/y", RepoName: "y", RepoOwner: "acme", AuthToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if b := f.mustFind("POST", "/repos/migrate").Body; b["clone_addr"] != "https://github.com/x/y" || b["repo_owner"] != "acme" || b["private"] != true || b["auth_token"] != "t" {
		t.Errorf("migrate body = %v", b)
	}

	bp := DefaultBranchProtection("main", 1, true, []string{"ci/test"})
	if err := c.EnsureBranchProtection(ctx, "acme", "api", bp); err != nil {
		t.Fatal(err)
	}
	got := f.mustFind("POST", "/repos/acme/api/branch_protections").Body
	want := map[string]any{
		"rule_name": "main", "enable_push": false, "require_signed_commits": true, "required_approvals": float64(1),
		"dismiss_stale_approvals": true, "block_on_rejected_reviews": true, "enable_merge_whitelist": true,
		"merge_whitelist_teams": []any{"mergers"}, "enable_status_check": true, "status_check_contexts": []any{"ci/test"},
		"apply_to_admins": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("protection body =\n%v\nwant\n%v", got, want)
	}
	if err := c.EnsureBranchProtection(ctx, "acme", "web", bp); err != nil {
		t.Fatal(err)
	}
	f.mustFind("PATCH", "/repos/acme/web/branch_protections/main")

	if err := c.EnsureTagProtection(ctx, "acme", "api", "v*", []string{"mergers"}); err != nil {
		t.Fatal(err)
	}
	if b := f.mustFind("POST", "/repos/acme/api/tag_protections").Body; b["name_pattern"] != "v*" {
		t.Errorf("tag body = %v", b)
	}
	if err := c.EnsurePushMirror(ctx, "acme", "api", "https://github.com/acme/api.git", "u", "p"); err != nil {
		t.Fatal(err)
	}
	if b := f.mustFind("POST", "/repos/acme/api/push_mirrors").Body; b["remote_address"] != "https://github.com/acme/api.git" || b["remote_password"] != "p" {
		t.Errorf("mirror body = %v", b)
	}
}

func TestSystemHook(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /admin/hooks": jsonResp(200, []Hook{}),
	})
	if err := c.EnsureSystemHook(context.Background(), "http://op/hook", "s", []string{"repository"}); err != nil {
		t.Fatal(err)
	}
	b := f.mustFind("POST", "/admin/hooks").Body
	if b["type"] != "forgejo" || b["config"].(map[string]any)["url"] != "http://op/hook" {
		t.Errorf("hook body = %v", b)
	}
}

func TestRunners(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /orgs/acme/actions/runners/jobs?labels=swarm%2Cswarm-dirty": jsonResp(200, []RunJob{
			{ID: 1, Status: "waiting", RunsOn: []string{"swarm"}, Handle: "h1"}, {ID: 2, Status: "running"},
		}),
		"POST /orgs/acme/actions/runners": jsonResp(201, RegisteredRunner{ID: 4, UUID: "u", Token: "t"}),
	})
	ctx := context.Background()
	js, err := c.WaitingJobs(ctx, "acme", []string{"swarm", "swarm-dirty"})
	if err != nil || len(js) != 1 || js[0].Handle != "h1" {
		t.Fatal(js, err)
	}
	r, err := c.RegisterOrgRunner(ctx, "acme", "pool-1", "d", true)
	if err != nil || r.UUID != "u" {
		t.Fatal(r, err)
	}
	if b := f.mustFind("POST", "/orgs/acme/actions/runners").Body; b["ephemeral"] != true || b["name"] != "pool-1" {
		t.Errorf("register body = %v", b)
	}
	if err := c.DeleteOrgRunner(ctx, "acme", 4); err != nil {
		t.Fatal(err)
	}
	f.mustFind("DELETE", "/orgs/acme/actions/runners/4")
}

func TestWaitingJobsNull(t *testing.T) {
	_, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /orgs/acme/actions/runners/jobs?labels=swarm": func(w http.ResponseWriter) { _, _ = w.Write([]byte("null")) },
	})
	js, err := c.WaitingJobs(context.Background(), "acme", []string{"swarm"})
	if err != nil || len(js) != 0 {
		t.Fatal(js, err)
	}
}

func TestAPIError(t *testing.T) {
	_, c := newFake(t, nil)
	_, err := c.GetUser(context.Background(), "nobody")
	if !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
}
