//go:build canary

package canary

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/ore"
	"github.com/usr-lex/swarmageddon/internal/supervisor/tmux"
)

var updateSchema = flag.Bool("update-schema", false, "rewrite testdata/ore-schema.json")

const oreScript = `
turns:
  - match: "MARK-FIRST"
    steps:
      - text: "ORE-FIRST-DONE"
  - match: "MARK-SLEEP"
    steps:
      - command: "sleep 6; echo slept"
      - text: "finished sleeping"
  - match: "MARK-TOUCH"
    steps:
      - command: "touch touched.txt"
      - text: "touched"
  - match: "MARK-MCP"
    steps:
      - mcp: {server: swarm, tool: send_message, input: {to: hermes, body: "hello from ore"}}
      - text: "sent it"
`

func oreBinary(t *testing.T) string {
	prefix := npmInstall(t, "@ore-cli/ore", OreVersion+"-linux-x64")
	p := filepath.Join(prefix, "node_modules/@ore-cli/ore/vendor/x86_64-unknown-linux-musl/bin/ore")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("ore binary not found: %v", err)
	}
	return p
}

func TestOre(t *testing.T) {
	bin := oreBinary(t)
	swarmBin := buildSwarm(t)
	m, url := mock(t, oreScript)
	home := shortTemp(t)
	work := filepath.Join(home, "work")
	codexHome := filepath.Join(home, ".ore")
	os.MkdirAll(work, 0o755)
	os.MkdirAll(filepath.Join(codexHome, "rules"), 0o755)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	var drv *ore.Driver
	r := newRoom(t, ctx, "ore", func(c *supervisor.Core) agent.Agent {
		drv = &ore.Driver{Home: home, WorkDir: work, CodexHome: codexHome, Events: c, Log: c.Log}
		return drv
	})
	policy := ore.Policy{
		GatewayURL: url + "/v1", WireAPI: "responses", Model: "gpt-mock",
		SwarmCommand: swarmBin, SwarmEnv: map[string]string{"SWARM_SOCKET": r.sock},
	}
	req, err := ore.Requirements(policy)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ore.Config(policy)
	if err != nil {
		t.Fatal(err)
	}
	rules := []byte(`prefix_rule(pattern=["touch"], decision="prompt", justification="canary: file creation needs a person")` + "\n")
	wrapper := etcOverlay(t, map[string][]byte{
		"codex/requirements.toml":   req,
		"codex/config.toml":         cfg,
		"codex/rules/persona.rules": rules,
	}, bin)
	drv.Binary = wrapper
	drv.Socket = filepath.Join(shortTemp(t), "ore.sock")
	drv.Env = []string{"HOME=" + home, ore.GatewayKeyEnv + "=canary-key"}
	drv.Settings = ore.UserSettings{Model: "gpt-mock"}
	drv.FirstTurn = "You have just started. MARK-FIRST"
	drv.DeveloperInstructions = "You are a canary."
	drv.Session = tmux.Session{Socket: "swarm-canary-ore"}
	defer drv.Session.Kill()

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { drv.Run(runCtx); close(done) }()

	t.Run("thread/start and first turn", func(t *testing.T) {
		waitFor(t, "thread", 90*time.Second, func() bool { return r.core.StatusReport().GetSessionId() != "" })
		waitFor(t, "first turn at the model", 60*time.Second, func() bool { return len(requestsWith(m, "MARK-FIRST", 0)) > 0 })
		waitFor(t, "idle", 60*time.Second, func() bool { return r.core.CurrentPresence() == messagingv1.Presence_PRESENCE_IDLE })
	})
	thread := r.core.StatusReport().GetSessionId()

	t.Run("TUI attaches with --remote", func(t *testing.T) {
		waitFor(t, "history in the TUI", 60*time.Second, func() bool {
			pane, _ := drv.Session.Capture()
			return strings.Contains(pane, "ORE-FIRST-DONE")
		})
	})

	t.Run("turn/start from idle, turn/steer mid-turn", func(t *testing.T) {
		r.fake.PushMessage("01SLEEP", "hermes@acme", "please run MARK-SLEEP")
		waitFor(t, "sleep command requested", 60*time.Second, func() bool { return len(requestsWith(m, "MARK-SLEEP", 0)) > 0 })
		r.fake.PushMessage("01STEER", "hermes@acme", "STEER-ORE-1 while you sleep")
		waitFor(t, "turn continued", 60*time.Second, func() bool { return len(requestsWith(m, "MARK-SLEEP", 1)) > 0 })
		if !strings.Contains(requestsWith(m, "MARK-SLEEP", 1)[0].Body, "STEER-ORE-1") {
			t.Error("steered message not in the continued turn's request")
		}
	})

	t.Run("approval request answered through messaging", func(t *testing.T) {
		r.fake.Answer = func(a *messagingv1.ApprovalRequest) *messagingv1.ApprovalAnswer {
			return &messagingv1.ApprovalAnswer{ApprovalId: a.GetId(), Decision: messagingv1.Decision_DECISION_ALLOW, AnsweredBy: "alice@acme"}
		}
		waitFor(t, "idle", 60*time.Second, func() bool { return r.core.CurrentPresence() == messagingv1.Presence_PRESENCE_IDLE })
		r.fake.PushMessage("01TOUCH", "alice@acme", "MARK-TOUCH please")
		waitFor(t, "touched.txt", 90*time.Second, func() bool { _, err := os.Stat(filepath.Join(work, "touched.txt")); return err == nil })
		_, _, _, _, approvals, _ := r.fake.Snapshot()
		if len(approvals) == 0 || approvals[0].GetTool() != "exec" || approvals[0].GetCli() != "ore" {
			t.Errorf("approvals %s", dump(approvals))
		}
	})

	t.Run("swarm MCP send_message", func(t *testing.T) {
		waitFor(t, "idle", 60*time.Second, func() bool { return r.core.CurrentPresence() == messagingv1.Presence_PRESENCE_IDLE })
		r.fake.PushMessage("01MCP", "alice@acme", "MARK-MCP")
		waitFor(t, "message sent through swarm mcp", 90*time.Second, func() bool {
			sent, _, _, _, _, _ := r.fake.Snapshot()
			for _, s := range sent {
				if s.GetBody() == "hello from ore" {
					return true
				}
			}
			return false
		})
	})

	t.Run("resume after restart", func(t *testing.T) {
		stop()
		<-done
		before := len(requestsWith(m, "MARK-FIRST", 0))
		runCtx, stop = context.WithCancel(ctx)
		done = make(chan struct{})
		go func() { drv.Run(runCtx); close(done) }()
		waitFor(t, "first turn after restart", 90*time.Second, func() bool { return len(requestsWith(m, "MARK-FIRST", 0)) > before })
		if got := r.core.StatusReport().GetSessionId(); got != thread {
			t.Errorf("thread after restart %q, want %q", got, thread)
		}
	})
	stop()
	<-done
}

// schemaFiles are the app-server schema files for the methods and events
// the supervisor uses (ADR 0007).
var schemaFiles = []string{
	"v2/ThreadStartParams.json", "v2/ThreadResumeParams.json", "v2/ThreadListParams.json", "v2/ThreadListResponse.json",
	"v2/TurnStartParams.json", "v2/TurnSteerParams.json", "v2/TurnSteerResponse.json",
	"v2/TurnStartedNotification.json", "v2/TurnCompletedNotification.json",
	"v2/ThreadStatusChangedNotification.json", "v2/ThreadTokenUsageUpdatedNotification.json",
	"v2/ServerRequestResolvedNotification.json",
	"CommandExecutionRequestApprovalParams.json", "CommandExecutionRequestApprovalResponse.json",
	"FileChangeRequestApprovalParams.json", "FileChangeRequestApprovalResponse.json",
}

// TestOreSchemaSnapshot fails when the pinned ore's protocol changes for the
// methods and events we use. Regenerate with -update-schema after review.
func TestOreSchemaSnapshot(t *testing.T) {
	bin := oreBinary(t)
	dir := t.TempDir()
	if out, err := exec.Command(bin, "app-server", "generate-json-schema", "--out", dir).CombinedOutput(); err != nil {
		t.Fatalf("generate-json-schema: %v %s", err, out)
	}
	got := map[string]any{}
	for _, f := range schemaFiles {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Errorf("schema file %s: %v", f, err)
			continue
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		got[f] = v
	}
	// The method tables: each method we call or handle must still exist.
	for file, methods := range map[string][]string{
		"ClientRequest.json":      {"initialize", "thread/start", "thread/resume", "thread/list", "turn/start", "turn/steer"},
		"ServerNotification.json": {"turn/started", "turn/completed", "thread/status/changed", "thread/tokenUsage/updated", "serverRequest/resolved", "item/started"},
		"ServerRequest.json":      {"item/commandExecution/requestApproval", "item/fileChange/requestApproval"},
	} {
		b, _ := os.ReadFile(filepath.Join(dir, file))
		var present []string
		for _, m := range methods {
			if strings.Contains(string(b), `"`+m+`"`) {
				present = append(present, m)
			}
		}
		sort.Strings(present)
		got[file+"#methods"] = present
	}
	b, _ := json.MarshalIndent(got, "", "  ")
	snap := filepath.Join("testdata", "ore-schema.json")
	if *updateSchema {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(snap, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("%v (run with -update-schema to create it)", err)
	}
	if string(want) != string(b) {
		t.Errorf("ore app-server schema changed for the methods we use; review the diff, update the supervisor, then run with -update-schema")
	}
}
