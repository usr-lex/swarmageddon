//go:build canary

package canary

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/claude"
	"github.com/usr-lex/swarmageddon/internal/supervisor/tmux"
)

const claudeScript = `
turns:
  - match: "MARK-FIRST"
    steps:
      - text: "first turn done"
  - match: "MARK-SLEEP"
    steps:
      - command: "sleep 6; echo slept"
      - text: "finished sleeping"
  - match: "MARK-TOUCH-TUI"
    steps:
      - command: "touch tui.txt"
      - text: "touched from tui"
  - match: "MARK-TOUCH"
    steps:
      - command: "touch touched.txt"
      - text: "touched"
  - match: "MARK-WARDEN-DENY"
    steps:
      - command: "curl -s -m 1 http://169.254.169.254/ || true"
      - text: "warden deny seen"
  - match: "MARK-WARDEN-ASK"
    steps:
      - command: "git push -f origin canary-branch || true"
      - text: "warden ask seen"
  - match: "MARK-MCP"
    steps:
      - mcp: {server: swarm, tool: send_message, input: {to: hermes, body: "hello from the canary"}}
      - text: "sent it"
`

// dialogMarkers are texts of dialogs that would block an unattended start.
var dialogMarkers = []string{
	"Choose the text style", "Do you trust the files", "trust this folder", "Yes, I trust",
	"Security notes", "Press Enter to continue", "Detected a custom API key", "Resume from summary",
	"Bypass Permissions mode", "Let's get started",
}

func assertNoDialog(t *testing.T, s *tmux.Session) {
	t.Helper()
	pane, err := s.Capture()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range dialogMarkers {
		if strings.Contains(pane, m) {
			t.Fatalf("dialog %q visible:\n%s", m, pane)
		}
	}
	if !strings.Contains(pane, "❯") {
		t.Fatalf("no prompt visible:\n%s", pane)
	}
}

func claudeBinary(t *testing.T) string {
	prefix := npmInstall(t, "@anthropic-ai/claude-code", ClaudeCodeVersion)
	for _, p := range []string{
		filepath.Join(prefix, "node_modules/@anthropic-ai/claude-code-linux-x64/claude"),
		filepath.Join(prefix, "node_modules/.bin/claude"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatal("claude binary not found in " + prefix)
	return ""
}

func TestClaudeCode(t *testing.T) {
	bin := claudeBinary(t)
	swarmBin := buildSwarm(t)
	m, url := mock(t, claudeScript)

	settings, err := claude.ManagedSettings(claude.ManagedPolicy{SwarmCommand: swarmBin, ApprovalTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	mcpJSON, _ := claude.ManagedMCP(claude.ManagedPolicy{SwarmCommand: swarmBin})
	wrapper := etcOverlay(t, map[string][]byte{
		"claude-code/managed-settings.json": settings,
		"claude-code/managed-mcp.json":      mcpJSON,
		"claude-code/CLAUDE.md":             []byte("# Canary\nYou are a canary agent.\n"),
	}, bin)

	home := shortTemp(t)
	work := filepath.Join(home, "work")
	os.MkdirAll(work, 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	var sockPath string
	newDriver := func(c *supervisor.Core) *claude.Driver {
		return &claude.Driver{
			Name: "athena", Binary: wrapper, Model: "claude-opus-5-5", PermissionMode: "default",
			Home: home, WorkDir: work,
			Env: []string{
				"HOME=" + home, "ANTHROPIC_BASE_URL=" + url, "ANTHROPIC_AUTH_TOKEN=canary-key",
				"SWARM_SOCKET=" + sockPath, "PATH=" + filepath.Dir(swarmBin) + ":" + os.Getenv("PATH"),
			},
			Session:   tmux.Session{Socket: "swarm-canary-claude"},
			FirstTurn: "You have just started. MARK-FIRST",
			Events:    c, Log: c.Log,
		}
	}
	var drv *claude.Driver
	r := newRoom(t, ctx, "claude", func(c *supervisor.Core) agent.Agent { drv = newDriver(c); return drv })
	sockPath = r.sock
	drv.Env[3] = "SWARM_SOCKET=" + r.sock
	defer drv.Session.Kill()

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { drv.Run(runCtx); close(done) }()

	t.Run("SessionStart reports socket and token", func(t *testing.T) {
		waitFor(t, "SessionStart", 90*time.Second, func() bool { return r.core.StatusReport().GetSessionId() != "" })
	})
	firstSession := r.core.StatusReport().GetSessionId()

	t.Run("no dialog on first start", func(t *testing.T) {
		time.Sleep(2 * time.Second)
		assertNoDialog(t, &drv.Session)
	})

	t.Run("first turn delivered", func(t *testing.T) {
		waitFor(t, "first turn at the model", 60*time.Second, func() bool { return len(requestsWith(m, "MARK-FIRST", 0)) > 0 })
	})

	t.Run("message from idle starts a turn and one mid-turn arrives in it", func(t *testing.T) {
		waitFor(t, "idle", 60*time.Second, func() bool { return r.core.CurrentPresence() == messagingv1.Presence_PRESENCE_IDLE })
		r.fake.PushMessage("01SLEEP", "hermes@acme", "please run MARK-SLEEP")
		waitFor(t, "sleep command requested", 60*time.Second, func() bool { return len(requestsWith(m, "MARK-SLEEP", 0)) > 0 })
		r.fake.PushMessage("01STEER", "hermes@acme", "STEER-CLAUDE-1 while you sleep")
		waitFor(t, "turn continued", 60*time.Second, func() bool { return len(requestsWith(m, "MARK-SLEEP", 1)) > 0 })
		req := requestsWith(m, "MARK-SLEEP", 1)[0]
		if !strings.Contains(req.Body, "STEER-CLAUDE-1") {
			t.Errorf("mid-turn message not in the continued turn's request")
		}
		if !strings.Contains(requestsWith(m, "MARK-SLEEP", 0)[0].Body, `from=\"hermes@acme\"`) {
			t.Errorf("envelope missing in request")
		}
		_, acks, presence, _, _, _ := r.fake.Snapshot()
		if len(acks) < 2 {
			t.Errorf("acks %v", acks)
		}
		var sawWorking bool
		for _, p := range presence {
			sawWorking = sawWorking || p == messagingv1.Presence_PRESENCE_WORKING
		}
		if !sawWorking {
			t.Errorf("presence never working: %v", presence)
		}
	})

	t.Run("PermissionRequest round trip through messaging", func(t *testing.T) {
		r.fake.Answer = func(a *messagingv1.ApprovalRequest) *messagingv1.ApprovalAnswer {
			if strings.Contains(a.GetSummary(), "touched.txt") {
				return &messagingv1.ApprovalAnswer{ApprovalId: a.GetId(), Decision: messagingv1.Decision_DECISION_ALLOW, AnsweredBy: "alice@acme"}
			}
			return nil
		}
		r.fake.PushMessage("01TOUCH", "alice@acme", "MARK-TOUCH please")
		waitFor(t, "touched.txt", 90*time.Second, func() bool { _, err := os.Stat(filepath.Join(work, "touched.txt")); return err == nil })
		_, _, _, _, approvals, _ := r.fake.Snapshot()
		if len(approvals) == 0 || approvals[0].GetTool() != "Bash" || approvals[0].GetCli() != "claude" {
			t.Errorf("approvals %s", dump(approvals))
		}
	})

	t.Run("approval answered in the TUI is withdrawn", func(t *testing.T) {
		waitFor(t, "idle", 60*time.Second, func() bool { return r.core.CurrentPresence() == messagingv1.Presence_PRESENCE_IDLE })
		r.fake.PushMessage("01TUI", "alice@acme", "MARK-TOUCH-TUI please")
		waitFor(t, "approval request", 60*time.Second, func() bool {
			_, _, _, _, ap, _ := r.fake.Snapshot()
			return len(ap) >= 2
		})
		time.Sleep(time.Second)
		drv.Session.SendKeys("Enter")
		waitFor(t, "tui.txt", 60*time.Second, func() bool { _, err := os.Stat(filepath.Join(work, "tui.txt")); return err == nil })
		waitFor(t, "withdrawal", 30*time.Second, func() bool { _, _, _, _, _, w := r.fake.Snapshot(); return len(w) > 0 })
	})

	t.Run("swarm MCP send_message", func(t *testing.T) {
		r.fake.Answer = func(a *messagingv1.ApprovalRequest) *messagingv1.ApprovalAnswer {
			return &messagingv1.ApprovalAnswer{ApprovalId: a.GetId(), Decision: messagingv1.Decision_DECISION_ALLOW}
		}
		r.fake.PushMessage("01MCP", "alice@acme", "MARK-MCP")
		defer func() {
			if t.Failed() {
				for _, q := range m.Requests() {
					if q.Turn == "MARK-MCP" {
						t.Logf("request step %d reply %s; swarm tool offered: %v", q.Step, q.Reply, strings.Contains(q.Body, "mcp__swarm__send_message"))
					}
				}
				pane, _ := drv.Session.Capture()
				t.Logf("pane:\n%s", pane)
			}
		}()
		waitFor(t, "message sent through swarm mcp", 90*time.Second, func() bool {
			sent, _, _, _, _, _ := r.fake.Snapshot()
			for _, s := range sent {
				if s.GetBody() == "hello from the canary" && s.GetTo() == "hermes" {
					return true
				}
			}
			return false
		})
	})

	r.core.Warden = supervisor.NewWarden(r.core.Room, r.core, "", "", work)

	t.Run("warden rule deny reaches the model", func(t *testing.T) {
		waitFor(t, "idle", 60*time.Second, func() bool { return r.core.CurrentPresence() == messagingv1.Presence_PRESENCE_IDLE })
		r.fake.PushMessage("01WDENY", "alice@acme", "MARK-WARDEN-DENY")
		waitFor(t, "denied call's result", 90*time.Second, func() bool { return len(requestsWith(m, "MARK-WARDEN-DENY", 1)) > 0 })
		if body := requestsWith(m, "MARK-WARDEN-DENY", 1)[0].Body; !strings.Contains(body, "Cloud metadata addresses are off limits") {
			t.Errorf("deny reason not in the tool result: %.2000s", body)
		}
		waitFor(t, "verdict", 30*time.Second, func() bool {
			for _, v := range r.fake.Verdicts() {
				if v.GetRule() == "cloud-metadata" && v.GetAction() == "denied" {
					return true
				}
			}
			return false
		})
	})

	wardenAsk := func(t *testing.T, id string) {
		var reason string
		r.fake.Answer = func(a *messagingv1.ApprovalRequest) *messagingv1.ApprovalAnswer {
			if strings.Contains(a.GetSummary(), "git push -f") {
				reason = a.GetReason()
				return &messagingv1.ApprovalAnswer{ApprovalId: a.GetId(), Decision: messagingv1.Decision_DECISION_DENY, Message: "no force pushes"}
			}
			return nil
		}
		waitFor(t, "idle", 60*time.Second, func() bool { return r.core.CurrentPresence() == messagingv1.Presence_PRESENCE_IDLE })
		before := len(requestsWith(m, "MARK-WARDEN-ASK", 1))
		r.fake.PushMessage(id, "alice@acme", "MARK-WARDEN-ASK "+id)
		waitFor(t, "escalated call's result", 90*time.Second, func() bool { return len(requestsWith(m, "MARK-WARDEN-ASK", 1)) > before })
		if !strings.Contains(reason, "Escalated by the warden") {
			t.Errorf("approval reason %q", reason)
		}
		if body := requestsWith(m, "MARK-WARDEN-ASK", 1)[before].Body; !strings.Contains(body, "no force pushes") {
			t.Errorf("person's denial not in the tool result: %.2000s", body)
		}
	}
	t.Run("warden escalation reaches the approvals route", func(t *testing.T) { wardenAsk(t, "01WASK") })

	t.Run("resume after restart, no dialog", func(t *testing.T) {
		stop()
		<-done
		before := len(requestsWith(m, "MARK-FIRST", 0))
		runCtx, stop = context.WithCancel(ctx)
		done = make(chan struct{})
		if args := strings.Join(drv.Args(), " "); !strings.Contains(args, "--resume "+firstSession) {
			t.Errorf("args %q", args)
		}
		go func() { drv.Run(runCtx); close(done) }()
		waitFor(t, "first turn after resume", 90*time.Second, func() bool { return len(requestsWith(m, "MARK-FIRST", 0)) > before })
		if got := r.core.StatusReport().GetSessionId(); got != firstSession {
			t.Errorf("resumed session %q, want %q", got, firstSession)
		}
		assertNoDialog(t, &drv.Session)
	})

	t.Run("warden escalation in bypassPermissions mode", func(t *testing.T) {
		stop()
		<-done
		drv.PermissionMode = "bypassPermissions"
		// Claude refuses bypass mode as root unless sandboxed; rooms run it as uid 1000.
		drv.Env = append(drv.Env, "IS_SANDBOX=1")
		before := len(requestsWith(m, "MARK-FIRST", 0))
		runCtx, stop = context.WithCancel(ctx)
		done = make(chan struct{})
		go func() { drv.Run(runCtx); close(done) }()
		waitFor(t, "first turn in bypass mode", 90*time.Second, func() bool { return len(requestsWith(m, "MARK-FIRST", 0)) > before })
		defer func() {
			if t.Failed() {
				pane, _ := drv.Session.Capture()
				t.Logf("pane:\n%s", pane)
			}
		}()
		wardenAsk(t, "01WASK2")
	})
	stop()
	<-done
}
