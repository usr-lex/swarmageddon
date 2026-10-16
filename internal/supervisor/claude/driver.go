package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/proc"
	"github.com/usr-lex/swarmageddon/internal/supervisor/tmux"
)

// Driver runs Claude Code in tmux and talks to it through hooks and the
// session's inbox socket.
type Driver struct {
	Name string // --name, the room name
	// Binary is the claude executable (default "claude").
	Binary         string
	Model          string
	Effort         string
	PermissionMode string
	// Home and WorkDir of the agent user; WorkDir is the CLI's cwd.
	Home, WorkDir string
	User          *proc.User
	// Env for the CLI (ANTHROPIC_BASE_URL, ANTHROPIC_AUTH_TOKEN, ...), added
	// to the fixed ones this driver sets.
	Env       []string
	ExtraArgs []string
	// Session is the tmux session; Name defaults to "agent".
	Session tmux.Session
	// StateDir keeps the last session ID (default ~/.swarm).
	StateDir  string
	FirstTurn string
	Events    agent.Events
	Log       *slog.Logger
	// MinBackoff between restarts (default 2 s).
	MinBackoff time.Duration

	mu      sync.Mutex
	socket  string
	token   string
	ready   chan struct{} // closed when the inbox socket is known
	runSeq  int           // increments per CLI start
	firstAt int           // runSeq for which the first turn was sent
	pending map[*context.CancelFunc]string
}

func (d *Driver) init() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ready == nil {
		d.ready = make(chan struct{})
	}
	if d.Binary == "" {
		d.Binary = "claude"
	}
	if d.Session.Name == "" {
		d.Session.Name = "agent"
	}
	if d.Session.User == nil {
		d.Session.User = d.User
	}
	if d.Session.Dir == "" {
		d.Session.Dir = d.WorkDir
	}
	if d.StateDir == "" {
		d.StateDir = filepath.Join(d.Home, ".swarm")
	}
	if d.Session.EnvFile == "" {
		d.Session.EnvFile = filepath.Join(d.StateDir, "agent.env")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
}

func (d *Driver) bypass() bool { return d.PermissionMode == "bypassPermissions" }

// Args is the claude command line for this start.
func (d *Driver) Args() []string {
	args := []string{d.Binary, "--name", d.Name}
	switch {
	case d.bypass():
		args = append(args, "--dangerously-skip-permissions")
	case d.PermissionMode != "":
		args = append(args, "--permission-mode", d.PermissionMode)
	}
	args = append(args, ResumeArgs(d.Home, d.WorkDir, d.lastSession())...)
	if d.Model != "" {
		args = append(args, "--model", d.Model)
	}
	return append(args, d.ExtraArgs...)
}

func (d *Driver) env() []string {
	env := []string{
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"DISABLE_AUTOUPDATER=1",
		"TERM=xterm-256color",
	}
	if d.Effort != "" {
		env = append(env, "CLAUDE_CODE_EFFORT_LEVEL="+d.Effort)
	}
	return append(env, d.Env...)
}

// Run implements agent.Agent.
func (d *Driver) Run(ctx context.Context) error {
	d.init()
	uid, gid := 0, 0
	if d.User != nil {
		uid, gid = int(d.User.UID), int(d.User.GID)
	}
	if err := mkdirOwned(d.StateDir, uid, gid); err != nil {
		return err
	}
	backoff := d.MinBackoff
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	for {
		trust := d.WorkDir
		if top := gitTop(d.WorkDir); top != "" {
			trust = top
		}
		if err := PrepareDialogs(d.Home, trust, d.bypass(), uid, gid); err != nil {
			d.Log.Error("pre-answering claude dialogs", "err", err)
		}
		if trust != d.WorkDir {
			_ = PrepareDialogs(d.Home, d.WorkDir, d.bypass(), uid, gid)
		}
		d.mu.Lock()
		d.runSeq++
		d.mu.Unlock()
		sess := d.Session
		sess.Env = d.env()
		argv := d.Args()
		d.Log.Info("starting claude", "args", strings.Join(argv, " "))
		start := time.Now()
		err := sess.Run(ctx, argv)
		d.resetSocket()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = errors.New("claude exited")
		}
		d.Events.Exited(err)
		if time.Since(start) > time.Minute {
			backoff = d.MinBackoff
			if backoff == 0 {
				backoff = 2 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (d *Driver) resetSocket() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.socket, d.token = "", ""
	select {
	case <-d.ready:
		d.ready = make(chan struct{})
	default:
	}
}

func gitTop(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (d *Driver) lastSession() string {
	b, _ := os.ReadFile(filepath.Join(d.StateDir, "claude-session"))
	return strings.TrimSpace(string(b))
}

// Deliver implements agent.Agent: one frame to the session's inbox socket.
func (d *Driver) Deliver(ctx context.Context, msgID, text string) error {
	d.init()
	d.mu.Lock()
	ready := d.ready
	d.mu.Unlock()
	select {
	case <-ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	d.mu.Lock()
	sock, tok := d.socket, d.token
	d.mu.Unlock()
	return WriteFrame(sock, tok, msgID, text)
}

// Frame is a user message frame for the inbox socket (undocumented; read
// from the 2.1.301 bundle and checked by the canary).
type Frame struct {
	Type     string       `json:"type"`
	Message  FrameMessage `json:"message"`
	MsgID    string       `json:"msg_id,omitempty"`
	Priority string       `json:"priority"`
}

// FrameMessage is the message inside a Frame.
type FrameMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// WriteFrame connects to a session's inbox socket, authenticates with the
// session token, and writes one user frame with priority "next".
func WriteFrame(socket, token, msgID, text string) error {
	if socket == "" {
		return errors.New("no inbox socket")
	}
	if text == "" {
		return errors.New("empty message")
	}
	c, err := net.DialTimeout("unix", strings.TrimPrefix(socket, "uds:"), 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var buf []byte
	if token != "" {
		b, _ := json.Marshal(map[string]string{"type": "auth", "token": token})
		buf = append(append(buf, b...), '\n')
	}
	b, err := json.Marshal(Frame{Type: "user", Message: FrameMessage{Role: "user", Content: text}, MsgID: msgID, Priority: "next"})
	if err != nil {
		return err
	}
	buf = append(append(buf, b...), '\n')
	if _, err := c.Write(buf); err != nil {
		return err
	}
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	return nil
}

// HookInput holds the hook payload fields the supervisor uses.
type HookInput struct {
	SessionID        string          `json:"session_id"`
	TranscriptPath   string          `json:"transcript_path"`
	Cwd              string          `json:"cwd"`
	HookEventName    string          `json:"hook_event_name"`
	Source           string          `json:"source"`
	Prompt           string          `json:"prompt"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	NotificationType string          `json:"notification_type"`
	Message          string          `json:"message"`
	// PermissionSuggestions are the "always allow" options (PermissionRequest).
	PermissionSuggestions []json.RawMessage `json:"permission_suggestions"`
}

// PermissionOutput is the PermissionRequest hook's answer.
type PermissionOutput struct {
	HookSpecificOutput struct {
		HookEventName string             `json:"hookEventName"`
		Decision      PermissionDecision `json:"decision"`
	} `json:"hookSpecificOutput"`
}

// PermissionDecision is allow or deny.
type PermissionDecision struct {
	Behavior           string            `json:"behavior"`
	Message            string            `json:"message,omitempty"`
	UpdatedPermissions []json.RawMessage `json:"updatedPermissions,omitempty"`
}

// Hook implements agent.Agent.
func (d *Driver) Hook(ctx context.Context, event string, payload []byte, env map[string]string) ([]byte, error) {
	d.init()
	var in HookInput
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &in); err != nil {
			return nil, fmt.Errorf("hook payload: %w", err)
		}
	}
	ev := d.Events
	switch event {
	case "PostToolUse", "PostToolUseFailure":
		d.resolveLocal(toolKey(in.ToolName, in.ToolInput))
	case "Stop", "StopFailure", "UserPromptSubmit", "SessionStart":
		d.resolveLocal("")
	}
	switch event {
	case "SessionStart":
		d.sessionStart(in, env)
	case "UserPromptSubmit":
		ev.Presence(messagingv1.Presence_PRESENCE_WORKING)
		if p := strings.TrimSpace(in.Prompt); p != "" && !strings.HasPrefix(p, "<cross-session-message") {
			ev.Task(p)
		}
	case "PreToolUse", "PostToolUse", "PostToolUseFailure":
		ev.Presence(messagingv1.Presence_PRESENCE_WORKING)
	case "Stop", "StopFailure":
		ev.Presence(messagingv1.Presence_PRESENCE_IDLE)
	case "Notification":
		switch in.NotificationType {
		case "permission_prompt", "elicitation_dialog", "agent_needs_input":
			ev.Presence(messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN)
		case "idle_prompt":
			ev.Presence(messagingv1.Presence_PRESENCE_IDLE)
		}
	case "PermissionRequest":
		return d.permission(ctx, in)
	}
	return nil, nil
}

func (d *Driver) sessionStart(in HookInput, env map[string]string) {
	sock, tok := env["CLAUDE_CODE_MESSAGING_SOCKET"], env["CLAUDE_CODE_MESSAGING_TOKEN"]
	if in.SessionID != "" {
		d.Events.Session(in.SessionID)
		_ = os.WriteFile(filepath.Join(d.StateDir, "claude-session"), []byte(in.SessionID+"\n"), 0o600)
	}
	d.Log.Info("claude session started", "session", in.SessionID, "source", in.Source, "socket", sock)
	if sock == "" {
		d.Log.Warn("SessionStart without CLAUDE_CODE_MESSAGING_SOCKET; messages can't be delivered")
		return
	}
	d.mu.Lock()
	d.socket, d.token = sock, tok
	select {
	case <-d.ready:
	default:
		close(d.ready)
	}
	first := d.firstAt != d.runSeq
	d.firstAt = d.runSeq
	d.mu.Unlock()
	d.Events.Presence(messagingv1.Presence_PRESENCE_IDLE)
	if first && d.FirstTurn != "" {
		// After the hook returns, so the session is up when the frame lands.
		go func() {
			if err := WriteFrame(sock, tok, "first-turn", d.FirstTurn); err != nil {
				d.Log.Error("first turn", "err", err)
			}
		}()
	}
}

func (d *Driver) permission(ctx context.Context, in HookInput) ([]byte, error) {
	d.Events.Presence(messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN)
	defer d.Events.Presence(messagingv1.Presence_PRESENCE_WORKING)
	a := agent.Approval{
		Tool:         in.ToolName,
		Summary:      Summarize(in.ToolName, in.ToolInput),
		Detail:       string(in.ToolInput),
		SessionScope: len(in.PermissionSuggestions) > 0,
	}
	// Claude doesn't stop this hook when the dialog is answered in the TUI
	// (checked with 2.1.301), so a later hook event for the same call, or
	// the turn moving on, cancels it, which withdraws the request.
	key := toolKey(in.ToolName, in.ToolInput)
	actx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	if d.pending == nil {
		d.pending = map[*context.CancelFunc]string{}
	}
	d.pending[&cancel] = key
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, &cancel)
		d.mu.Unlock()
		cancel()
	}()
	dec, err := d.Events.Approve(actx, a)
	if err != nil {
		if ctx.Err() == nil && actx.Err() != nil {
			return nil, nil // answered in the terminal
		}
		return nil, err
	}
	var out PermissionOutput
	out.HookSpecificOutput.HookEventName = "PermissionRequest"
	if dec.Allow {
		out.HookSpecificOutput.Decision.Behavior = "allow"
		if dec.ForSession {
			out.HookSpecificOutput.Decision.UpdatedPermissions = sessionRules(in.PermissionSuggestions)
		}
	} else {
		out.HookSpecificOutput.Decision.Behavior = "deny"
		out.HookSpecificOutput.Decision.Message = dec.Message
		if out.HookSpecificOutput.Decision.Message == "" {
			out.HookSpecificOutput.Decision.Message = "Denied by a person."
		}
	}
	return json.Marshal(out)
}

// toolKey identifies a tool call by name and normalized input.
func toolKey(tool string, input json.RawMessage) string {
	var v any
	if json.Unmarshal(input, &v) == nil {
		if b, err := json.Marshal(v); err == nil {
			return tool + " " + string(b)
		}
	}
	return tool + " " + string(input)
}

// resolveLocal cancels pending permission requests for key ("" for all).
func (d *Driver) resolveLocal(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for c, k := range d.pending {
		if key == "" || k == key {
			(*c)()
		}
	}
}

// sessionRules keeps the suggestions that remember an allow (rules and
// directories) and drops mode changes such as setMode acceptEdits, which
// would widen more than this one approval.
func sessionRules(sugg []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	for _, s := range sugg {
		var t struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(s, &t)
		if t.Type == "addRules" || t.Type == "addDirectories" {
			out = append(out, s)
		}
	}
	return out
}

// Summarize makes a one-line summary of a tool call.
func Summarize(tool string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	for _, k := range []string{"command", "file_path", "path", "url", "pattern", "prompt"} {
		if v, ok := m[k].(string); ok && v != "" {
			return oneLine(tool + ": " + v)
		}
	}
	return oneLine(tool + " " + string(input))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}
