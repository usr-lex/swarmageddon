package wizard

import (
	"slices"
	"strings"
	"testing"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

func TestRepoCredentials(t *testing.T) {
	p := &ccv1.WorkspacePlan{Name: "acme", Org: "acme", Repositories: []*ccv1.RepositoryPlan{
		{Name: "api", ImportUrl: "https://github.com/acme/api.git", ImportCredential: "bot:ghp_import",
			PushMirror: "https://github.com/acme/api-mirror.git", MirrorToken: "ghp_mirror"},
		{Name: "web"},
		{Name: "docs", ImportUrl: "https://example.com/docs.git", ImportSecretName: "kept", PushMirror: "https://x/docs.git", MirrorSecretName: "kept-mirror"},
	}}
	creds := StageRepoCredentials(p)
	if len(creds) != 2 {
		t.Fatalf("creds %+v", creds)
	}
	imp, mir := creds[0], creds[1]
	if imp.Repo != "api" || imp.Kind != "import" || imp.Username != "bot" || imp.Token != "ghp_import" || !strings.HasPrefix(imp.SecretName, "repo-api-import-") {
		t.Errorf("import %+v", imp)
	}
	if mir.Kind != "mirror" || mir.Username != "" || mir.Token != "ghp_mirror" || !strings.HasPrefix(mir.SecretName, "repo-api-mirror-") {
		t.Errorf("mirror %+v", mir)
	}

	d := Build(p, Current{})
	repos := d.Workspace.Spec.Repositories
	if repos[0].ImportSecretName != imp.SecretName || repos[0].MirrorSecretName != mir.SecretName {
		t.Fatalf("api secrets %+v", repos[0])
	}
	if repos[1].ImportSecretName != "" || repos[1].MirrorSecretName != "" || repos[2].ImportSecretName != "kept" || repos[2].MirrorSecretName != "kept-mirror" {
		t.Fatalf("other repos %+v", repos[1:])
	}
	// Reading the plan back keeps the Secrets' names but never the values,
	// and applying it again without a new credential keeps them.
	back := FromCluster(Current{Workspace: d.Workspace})
	if r := back.Repositories[0]; r.ImportCredential != "" || r.MirrorToken != "" || r.ImportSecretName != imp.SecretName {
		t.Fatalf("round trip %+v", r)
	}
	if StageRepoCredentials(back) != nil {
		t.Fatal("a plan without credentials staged some")
	}
	if Build(back, Current{Workspace: d.Workspace}).Workspace.Spec.Repositories[0].MirrorSecretName != mir.SecretName {
		t.Fatal("re-apply lost the mirror secret")
	}
}

func TestRecordKeystrokes(t *testing.T) {
	p := &ccv1.WorkspacePlan{Name: "acme", Org: "acme", RecordKeystrokes: true}
	d := Build(p, Current{})
	if !d.Workspace.Spec.RecordKeystrokes || !FromCluster(Current{Workspace: d.Workspace}).RecordKeystrokes {
		t.Fatal("recordKeystrokes lost")
	}
	p.RecordKeystrokes = false
	if Build(p, Current{Workspace: d.Workspace}).Workspace.Spec.RecordKeystrokes {
		t.Fatal("recordKeystrokes not turned off")
	}
}

func TestValidateRepos(t *testing.T) {
	p := &ccv1.WorkspacePlan{Name: "acme", Org: "acme", Repositories: []*ccv1.RepositoryPlan{
		{Name: "ok", ImportUrl: "https://x/ok.git", ImportCredential: "user:tok"},
		{Name: "bad name"},
		{Name: "nourl", MirrorToken: "t"},
		{Name: "empty", ImportUrl: "https://x/e.git", ImportCredential: "user:"},
	}}
	got := strings.Join(validateRepos(p), "\n")
	for _, want := range []string{`"bad name": the name`, `"nourl": a mirror token needs its URL`, `"empty": the import credential is not`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"ok"`) {
		t.Errorf("valid repository refused:\n%s", got)
	}
}

// Model access (OpenShift report, feature 8): a workspace default and a
// per-room override, both ways through the plan; unknown values refused.
func TestModelAccess(t *testing.T) {
	p := &ccv1.WorkspacePlan{Name: "acme", Org: "acme", ModelAccess: "direct", Agents: []*ccv1.AgentPlan{
		{Name: "athena", Role: "implementer", Cli: "claude"},
		{Name: "hermes", Role: "implementer", Cli: "ore", ModelAccess: "gateway"},
	}}
	d := Build(p, Current{})
	if d.Workspace.Spec.Models.Access != "direct" {
		t.Fatalf("workspace access %q", d.Workspace.Spec.Models.Access)
	}
	got := map[string]string{}
	for _, r := range d.Rooms {
		got[r.Name] = string(r.Spec.ModelAccess)
	}
	if got["athena"] != "" || got["hermes"] != "gateway" {
		t.Fatalf("rooms %v", got)
	}
	var rooms []v1alpha1.Room
	for _, r := range d.Rooms {
		rooms = append(rooms, *r)
	}
	back := FromCluster(Current{Workspace: d.Workspace, Rooms: rooms})
	access := map[string]string{}
	for _, a := range back.Agents {
		access[a.GetName()] = a.GetModelAccess()
	}
	if back.ModelAccess != "direct" || access["hermes"] != "gateway" || access["athena"] != "" {
		t.Fatalf("round trip: %q %v", back.ModelAccess, access)
	}
	p.ModelAccess = "proxy"
	if probs := Validate(p, map[string]bool{}); !slices.ContainsFunc(probs, func(s string) bool { return strings.Contains(s, "model access") }) {
		t.Fatalf("bad access accepted: %v", probs)
	}
}

// A GitHub import (ADR 0012): repositories with their history, and bot
// accounts mapped to rooms of the plan, both ways through the plan.
func TestGitHubImport(t *testing.T) {
	p := &ccv1.WorkspacePlan{Name: "acme", Org: "acme",
		Agents: []*ccv1.AgentPlan{{Name: "athena", Role: "implementer", Cli: "claude"}},
		Repositories: []*ccv1.RepositoryPlan{{Name: "api", ImportUrl: "https://github.com/acme/api.git", ImportCredential: "github_pat_x", ImportGithub: true}},
		GithubAccounts: []*ccv1.GitHubAccountPlan{{Login: "bot-athena", Id: 11, Room: "athena"}},
	}
	if probs := Validate(p, map[string]bool{}); len(probs) != 0 {
		t.Fatalf("valid import refused: %v", probs)
	}
	StageRepoCredentials(p)
	d := Build(p, Current{})
	s := d.Workspace.Spec
	if !s.Repositories[0].ImportGitHub || len(s.GitHubAccounts) != 1 || s.GitHubAccounts[0] != (v1alpha1.GitHubAccount{Login: "bot-athena", ID: 11, Room: "athena"}) {
		t.Fatalf("spec %+v %+v", s.Repositories, s.GitHubAccounts)
	}
	back := FromCluster(Current{Workspace: d.Workspace})
	if !back.Repositories[0].ImportGithub || len(back.GithubAccounts) != 1 || back.GithubAccounts[0].GetId() != 11 {
		t.Fatalf("round trip %+v %+v", back.Repositories, back.GithubAccounts)
	}

	bad := &ccv1.WorkspacePlan{Name: "acme", Org: "acme",
		Repositories:   []*ccv1.RepositoryPlan{{Name: "api", ImportGithub: true}},
		GithubAccounts: []*ccv1.GitHubAccountPlan{{Login: "bot-zeus", Id: 0, Room: "zeus"}},
	}
	probs := strings.Join(Validate(bad, map[string]bool{}), "; ")
	for _, want := range []string{"needs a github.com import URL", "bot-zeus: no room zeus", "bot-zeus: GitHub user ID is required"} {
		if !strings.Contains(probs, want) {
			t.Errorf("problems lack %q: %s", want, probs)
		}
	}
}
