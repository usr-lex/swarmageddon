package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

func TestManagedSettings(t *testing.T) {
	b, err := ManagedSettings(ManagedPolicy{
		Allow: []string{"Bash(npm test)"}, Deny: []string{"Bash(rm -rf /)"}, ManagedRulesOnly: true,
		ExtraHooks:      json.RawMessage(`{"Stop":[{"hooks":[{"type":"command","command":"echo persona"}]}]}`),
		ApprovalTimeout: time.Hour,
		Env:             map[string]string{"ANTHROPIC_BASE_URL": "http://gw:4000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string
				Timeout int
			}
		}
		AllowManagedHooksOnly           bool
		AllowManagedPermissionRulesOnly bool
		CrossSessionInbound             string
		Permissions                     struct{ Allow, Deny []string }
		Env                             map[string]string
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for _, ev := range HookEvents {
		if len(s.Hooks[ev]) == 0 || s.Hooks[ev][0].Hooks[0].Command != "swarm hook "+ev {
			t.Errorf("hook %s: %+v", ev, s.Hooks[ev])
		}
	}
	if s.Hooks["PermissionRequest"][0].Hooks[0].Timeout != 3600 {
		t.Error("permission timeout")
	}
	if len(s.Hooks["Stop"]) != 2 || s.Hooks["Stop"][1].Hooks[0].Command != "echo persona" {
		t.Error("persona hooks not appended after ours")
	}
	if !s.AllowManagedHooksOnly || !s.AllowManagedPermissionRulesOnly || s.CrossSessionInbound != "accept" {
		t.Error("lock-down keys")
	}
	if s.Permissions.Allow[0] != "Bash(npm test)" || s.Env["ANTHROPIC_BASE_URL"] == "" {
		t.Error("permissions/env")
	}
	if _, err := ManagedSettings(ManagedPolicy{ExtraHooks: json.RawMessage(`[1]`)}); err == nil {
		t.Error("bad hooks accepted")
	}
	m, _ := ManagedMCP(ManagedPolicy{})
	if !strings.Contains(string(m), `"command": "swarm"`) || !strings.Contains(string(m), `"mcp"`) {
		t.Errorf("mcp: %s", m)
	}
}

func TestPrepareDialogsAndResume(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"keep":1,"projects":{"/w":{"x":2}}}`), 0o600)
	if err := PrepareDialogs(home, "/w", true, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	b, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	json.Unmarshal(b, &d)
	p := d["projects"].(map[string]any)["/w"].(map[string]any)
	if d["hasCompletedOnboarding"] != true || d["resumeReturnDismissed"] != true || p["hasTrustDialogAccepted"] != true || p["x"] != 2.0 || d["keep"] != 1.0 {
		t.Errorf("claude.json: %s", b)
	}
	s, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if !strings.Contains(string(s), "skipDangerousModePermissionPrompt") {
		t.Error("bypass prompt not skipped")
	}
	PrepareDialogs(home, "/w", false, os.Getuid(), os.Getgid())
	s, _ = os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if strings.Contains(string(s), "skipDangerousModePermissionPrompt") {
		t.Error("skip left on without bypass")
	}

	if ResumeArgs(home, "/home/a/work/api", "") != nil {
		t.Error("resume without transcripts")
	}
	dir := ProjectDir(home, "/home/a/work/api")
	if filepath.Base(dir) != "-home-a-work-api" {
		t.Error(dir)
	}
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "s1.jsonl"), nil, 0o600)
	if got := ResumeArgs(home, "/home/a/work/api", "s1"); strings.Join(got, " ") != "--resume s1" {
		t.Error(got)
	}
	if got := ResumeArgs(home, "/home/a/work/api", "gone"); strings.Join(got, " ") != "--continue" {
		t.Error(got)
	}
}

func TestArgs(t *testing.T) {
	d := &Driver{Name: "athena", Model: "m", PermissionMode: "bypassPermissions", Home: t.TempDir(), WorkDir: "/x"}
	d.init()
	if got := strings.Join(d.Args(), " "); got != "claude --name athena --dangerously-skip-permissions --model m" {
		t.Error(got)
	}
	d.PermissionMode = "acceptEdits"
	if got := strings.Join(d.Args(), " "); got != "claude --name athena --permission-mode acceptEdits --model m" {
		t.Error(got)
	}
}

type fakeEvents struct {
	mu       sync.Mutex
	presence []messagingv1.Presence
	task     string
	session  string
	approve  func(context.Context, agent.Approval) (agent.Decision, error)
}

func (f *fakeEvents) Presence(p messagingv1.Presence) {
	f.mu.Lock()
	f.presence = append(f.presence, p)
	f.mu.Unlock()
}
func (f *fakeEvents) Task(s string)        { f.mu.Lock(); f.task = s; f.mu.Unlock() }
func (f *fakeEvents) Tokens(_, _, _ int64) {}
func (f *fakeEvents) Session(id string)    { f.mu.Lock(); f.session = id; f.mu.Unlock() }
func (f *fakeEvents) Exited(error)         {}
func (f *fakeEvents) Approve(ctx context.Context, a agent.Approval) (agent.Decision, error) {
	return f.approve(ctx, a)
}
func (f *fakeEvents) last() messagingv1.Presence {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.presence[len(f.presence)-1]
}

// inbox is a fake Claude inbox socket recording received lines.
func inbox(t *testing.T) (string, chan []string) {
	sock := filepath.Join(t.TempDir(), "in.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	got := make(chan []string, 10)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			var lines []string
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				lines = append(lines, sc.Text())
			}
			c.Close()
			got <- lines
		}
	}()
	return sock, got
}

func TestHooksAndDelivery(t *testing.T) {
	sock, got := inbox(t)
	ev := &fakeEvents{approve: func(ctx context.Context, a agent.Approval) (agent.Decision, error) {
		if a.Tool != "Bash" || a.Summary != "Bash: rm x" || !a.SessionScope {
			t.Errorf("approval %+v", a)
		}
		return agent.Decision{Allow: true, ForSession: true}, nil
	}}
	d := &Driver{Name: "athena", Home: t.TempDir(), FirstTurn: "start working", Events: ev}
	d.init()
	os.MkdirAll(d.StateDir, 0o700)
	d.runSeq = 1

	delivered := make(chan error, 1)
	go func() { delivered <- d.Deliver(context.Background(), "m1", "hello") }()
	select {
	case <-delivered:
		t.Fatal("delivered before the session started")
	case <-time.After(50 * time.Millisecond):
	}

	out, err := d.Hook(context.Background(), "SessionStart", []byte(`{"session_id":"s-1","source":"startup"}`),
		map[string]string{"CLAUDE_CODE_MESSAGING_SOCKET": sock, "CLAUDE_CODE_MESSAGING_TOKEN": "tok"})
	if err != nil || out != nil {
		t.Fatal(out, err)
	}
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 {
		lines := <-got
		if len(lines) != 2 || lines[0] != `{"token":"tok","type":"auth"}` {
			t.Fatalf("lines %q", lines)
		}
		var f Frame
		json.Unmarshal([]byte(lines[1]), &f)
		if f.Type != "user" || f.Priority != "next" || f.Message.Role != "user" {
			t.Errorf("frame %s", lines[1])
		}
		seen[f.Message.Content] = true
	}
	if !seen["hello"] || !seen["start working"] {
		t.Errorf("seen %v", seen)
	}
	if ev.session != "s-1" {
		t.Error("session not reported")
	}
	// A second SessionStart in the same run (e.g. /clear) sends no new first turn.
	d.Hook(context.Background(), "SessionStart", []byte(`{"session_id":"s-2","source":"clear"}`), map[string]string{"CLAUDE_CODE_MESSAGING_SOCKET": sock})
	select {
	case l := <-got:
		t.Errorf("unexpected frame %q", l)
	case <-time.After(100 * time.Millisecond):
	}

	d.Hook(context.Background(), "UserPromptSubmit", []byte(`{"prompt":"fix the bug"}`), nil)
	if ev.last() != messagingv1.Presence_PRESENCE_WORKING || ev.task != "fix the bug" {
		t.Error("prompt")
	}
	d.Hook(context.Background(), "Stop", []byte(`{}`), nil)
	if ev.last() != messagingv1.Presence_PRESENCE_IDLE {
		t.Error("stop")
	}
	d.Hook(context.Background(), "Notification", []byte(`{"notification_type":"permission_prompt"}`), nil)
	if ev.last() != messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN {
		t.Error("notification")
	}
	out, err = d.Hook(context.Background(), "PermissionRequest", []byte(`{"tool_name":"Bash","tool_input":{"command":"rm x"},"permission_suggestions":[{"type":"addRules"},{"type":"setMode","mode":"acceptEdits"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedPermissions":[{"type":"addRules"}]}}}`
	if string(out) != want {
		t.Errorf("got %s", out)
	}
	ev.approve = func(context.Context, agent.Approval) (agent.Decision, error) {
		return agent.Decision{Message: "no"}, nil
	}
	out, _ = d.Hook(context.Background(), "PermissionRequest", []byte(`{"tool_name":"Bash","tool_input":{}}`), nil)
	if !strings.Contains(string(out), `"behavior":"deny","message":"no"`) {
		t.Errorf("deny %s", out)
	}
}
