package wizard

import (
	"strings"
	"testing"

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
