package roomcfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndDefaults(t *testing.T) {
	f := filepath.Join(t.TempDir(), "room.json")
	os.WriteFile(f, []byte(`{"room":"athena","workspace":"acme","cli":"claude","repos":["acme/api","acme/web.git"]}`), 0o644)
	r, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if r.AddressOrDefault() != "athena@acme" {
		t.Errorf("address %q", r.AddressOrDefault())
	}
	if got := r.WorkPath("/home/athena"); got != "/home/athena/work/api" {
		t.Errorf("work path %q", got)
	}
	if n, e := r.GitIdentity(); n != "Athena" || e != "athena@agents.acme.invalid" {
		t.Errorf("identity %q %q", n, e)
	}
	if !r.SudoEnabled() || r.FirstTurnPrompt() != DefaultFirstTurn {
		t.Error("defaults")
	}
	if RepoName("acme/web.git") != "web" {
		t.Error("repo name")
	}
}

func TestValidate(t *testing.T) {
	for _, r := range []Room{
		{Room: "Bad", Workspace: "w", CLI: "claude"},
		{Room: "a", Workspace: "", CLI: "claude"},
		{Room: "a", Workspace: "w", CLI: "vim"},
		{Room: "a", Workspace: "w", CLI: "ore", Repos: []string{"x"}},
		{Room: "a", Workspace: "w", CLI: "ore", WorkDir: "../etc"},
	} {
		if r.Validate() == nil {
			t.Errorf("%+v: want error", r)
		}
	}
}
