package advisor

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/mockmodel"
	"github.com/usr-lex/swarmageddon/internal/supervisor/llm"
)

func TestAdviseWithToolLoopAndLimit(t *testing.T) {
	sc, _ := mockmodel.ParseScript([]byte(`
turns:
  - match: "ADVISE-ME"
    steps:
      - tool: {name: read_file, input: {path: notes.txt}}
      - tool: {name: read_file, input: {path: ../outside.txt}}
      - text: "Use the second approach."
`))
	m := mockmodel.New(sc)
	srv := httptest.NewServer(m)
	defer srv.Close()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	os.MkdirAll(work, 0o755)
	os.WriteFile(filepath.Join(work, "notes.txt"), []byte("SECRET-PLAN in the notes"), 0o644)
	os.WriteFile(filepath.Join(root, "outside.txt"), []byte("OUTSIDE"), 0o644)
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644)

	now := time.Now()
	a := &Advisor{
		LLM:        &llm.Client{BaseURL: srv.URL, Key: "k", Model: "advisor-model"},
		MaxPerHour: 2, WorkDir: work, RoleText: "You review code.",
		Transcript: func(context.Context, int) string { return "user: fix the bug\nassistant: looking" },
		Task:       func() string { return "fixing #7" },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:        func() time.Time { return now },
	}
	ans, left, err := a.Advise(context.Background(), "ADVISE-ME which approach?", []string{"main.go", "/etc/passwd"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if ans != "Use the second approach." || left != 1 {
		t.Errorf("answer %q left %d", ans, left)
	}
	reqs := m.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests %d", len(reqs))
	}
	first := reqs[0].Body
	for _, want := range []string{"You review code.", "fixing #7", "fix the bug", "package main", "outside the work folder"} {
		if !strings.Contains(first, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if !strings.Contains(reqs[1].Body, "SECRET-PLAN") || !strings.Contains(reqs[2].Body, "outside the work folder") || strings.Contains(reqs[2].Body, "OUTSIDE") {
		t.Error("tool results wrong")
	}
	if _, _, err := a.Advise(context.Background(), "ADVISE-ME again", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Advise(context.Background(), "ADVISE-ME third", nil, false); err != ErrRateLimited {
		t.Errorf("want rate limit, got %v", err)
	}
	now = now.Add(61 * time.Minute)
	if _, _, err := a.Advise(context.Background(), "ADVISE-ME later", nil, false); err != nil {
		t.Errorf("after an hour: %v", err)
	}
}

func TestGrepAndListConfined(t *testing.T) {
	work := t.TempDir()
	os.MkdirAll(filepath.Join(work, "pkg"), 0o755)
	os.WriteFile(filepath.Join(work, "pkg", "a.go"), []byte("func Foo() {}\nfunc Bar() {}\n"), 0o644)
	os.Symlink("/etc", filepath.Join(work, "etc"))
	a := &Advisor{WorkDir: work}
	out := a.runTool("grep", `{"pattern":"func Ba"}`)
	if !strings.Contains(out, "pkg/a.go:2:") {
		t.Errorf("grep: %s", out)
	}
	if out := a.runTool("list_dir", `{}`); !strings.Contains(out, "pkg/") {
		t.Errorf("list: %s", out)
	}
	if out := a.runTool("read_file", `{"path":"etc/hostname"}`); !strings.HasPrefix(out, "error: outside") {
		t.Errorf("symlink escape: %s", out)
	}
}
