package ore

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

// Driver owns `ore app-server` and is its first client; tmux runs the ore
// TUI attached to the same thread with --remote.
type Driver struct {
	// Binary is the ore executable (default "ore").
	Binary string
	// CodexHome is CODEX_HOME (default ~/.ore).
	CodexHome string
	Home      string
	WorkDir   string
	// Socket is the app-server socket (default /run/swarm/ore.sock).
	Socket string
	User   *proc.User
	// Env for the app-server and TUI (SWARM_GATEWAY_KEY, ...).
	Env []string
	// Settings are merged into ~/.ore/config.toml before every start
	// (WorkDir trust is always added).
	Settings              UserSettings
	DeveloperInstructions string
	FirstTurn             string
	// Session is the TUI's tmux session; Name defaults to "agent".
	Session tmux.Session
	Events  agent.Events
	Log     *slog.Logger
	// MinBackoff between restarts (default 2 s).
	MinBackoff time.Duration

	mu         sync.Mutex
	stop       context.CancelFunc // ends the running app-server; nil between runs
	restart    bool               // the app-server was stopped by Restart
	client     *Client
	thread     string
	activeTurn string
	ready      chan struct{}
	approvals  map[string]context.CancelFunc
	pendingN   int
	items      map[string]json.RawMessage
}

func (d *Driver) init() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ready == nil {
		d.ready = make(chan struct{})
		d.approvals = map[string]context.CancelFunc{}
		d.items = map[string]json.RawMessage{}
	}
	if d.Binary == "" {
		d.Binary = "ore"
	}
	if d.CodexHome == "" {
		d.CodexHome = filepath.Join(d.Home, ".ore")
	}
	if d.Socket == "" {
		d.Socket = "/run/swarm/ore.sock"
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
	if d.Session.EnvFile == "" {
		d.Session.EnvFile = filepath.Join(d.Home, ".swarm", "agent.env")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
}

func (d *Driver) env() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string{"CODEX_HOME=" + d.CodexHome}, d.Env...)
}

// Restart implements agent.Restarter.
func (d *Driver) Restart(vars map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Env = agent.SetEnv(d.Env, vars)
	if d.stop != nil {
		d.restart = true
		d.stop()
	}
}

func (d *Driver) ids() (int, int) {
	if d.User == nil {
		return os.Getuid(), os.Getgid()
	}
	return int(d.User.UID), int(d.User.GID)
}

// Run implements agent.Agent.
func (d *Driver) Run(ctx context.Context) error {
	d.init()
	minB := d.MinBackoff
	if minB == 0 {
		minB = 2 * time.Second
	}
	backoff := minB
	for {
		start := time.Now()
		err := d.runOnce(ctx)
		d.reset()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.mu.Lock()
		restarted := d.restart
		d.restart = false
		d.mu.Unlock()
		if restarted {
			d.Log.Info("restarting ore with new settings")
			continue
		}
		d.Log.Warn("ore app-server ended", "err", err)
		d.Events.Exited(err)
		if time.Since(start) > time.Minute {
			backoff = minB
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func (d *Driver) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.client, d.thread, d.activeTurn = nil, "", ""
	select {
	case <-d.ready:
		d.ready = make(chan struct{})
	default:
	}
	for id, cancel := range d.approvals {
		cancel()
		delete(d.approvals, id)
	}
	d.pendingN = 0
	clear(d.items)
}

func (d *Driver) runOnce(ctx context.Context) error {
	uid, gid := d.ids()
	if err := os.MkdirAll(d.CodexHome, 0o700); err != nil {
		return err
	}
	if os.Getuid() == 0 {
		_ = os.Chown(d.CodexHome, uid, gid)
		_ = os.MkdirAll(filepath.Dir(d.Session.EnvFile), 0o700)
		_ = os.Chown(filepath.Dir(d.Session.EnvFile), uid, gid)
	} else {
		_ = os.MkdirAll(filepath.Dir(d.Session.EnvFile), 0o700)
	}
	s := d.Settings
	s.TrustDirs = append([]string{d.WorkDir}, s.TrustDirs...)
	if top := gitTop(d.WorkDir); top != "" && top != d.WorkDir {
		s.TrustDirs = append(s.TrustDirs, top)
	}
	if err := UpdateUserConfig(filepath.Join(d.CodexHome, "config.toml"), s, uid, gid); err != nil {
		return fmt.Errorf("ore config: %w", err)
	}
	_ = os.Remove(d.Socket)

	runCtx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.stop = cancel
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.stop = nil
		d.mu.Unlock()
		cancel()
	}()
	spec := proc.Spec{
		Name: "ore-app-server", Path: d.Binary,
		Args: []string{"app-server", "--listen", "unix://" + d.Socket},
		Env:  d.env(), Dir: d.WorkDir, User: d.User,
	}
	cmd := spec.Command(runCtx)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		select {
		case <-exited:
		default:
			proc.Stop(cmd, exited, 10*time.Second)
		}
	}()

	c, err := d.connect(runCtx, exited)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	c.OnNotification = d.onNotification
	c.OnRequest = func(id json.RawMessage, method string, params json.RawMessage) {
		d.onRequest(runCtx, c, id, method, params)
	}
	readDone := make(chan error, 1)
	go func() { readDone <- c.Run(runCtx) }()

	thread, err := d.openThread(runCtx, c)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.client, d.thread = c, thread
	close(d.ready)
	d.mu.Unlock()
	d.Events.Session(thread)
	d.Events.Presence(messagingv1.Presence_PRESENCE_IDLE)
	d.Log.Info("ore thread ready", "thread", thread)

	if d.FirstTurn != "" {
		if err := d.Deliver(runCtx, "first-turn", d.FirstTurn); err != nil {
			return fmt.Errorf("first turn: %w", err)
		}
	}
	go d.runTUI(runCtx, thread)

	select {
	case err := <-readDone:
		return fmt.Errorf("app-server connection: %w", err)
	case err := <-exited:
		return fmt.Errorf("app-server exited: %v", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// connect waits for the socket to accept connections and initializes.
func (d *Driver) connect(ctx context.Context, exited <-chan error) (*Client, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if conn, err := net.Dial("unix", d.Socket); err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("app-server socket did not appear")
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("app-server exited at start: %v", err)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return Dial(ctx, d.Socket)
}

func (d *Driver) openThread(ctx context.Context, c *Client) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := c.Call(callCtx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "swarm-supervisor", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	if err := c.Notify(callCtx, "initialized", nil); err != nil {
		return "", err
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	err = c.Call(callCtx, "thread/list", map[string]any{
		"cwd": d.WorkDir, "sortKey": "updated_at", "limit": 1, "sourceKinds": []string{},
	}, &list)
	if err != nil {
		return "", fmt.Errorf("thread/list: %w", err)
	}
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if len(list.Data) > 0 {
		err = c.Call(callCtx, "thread/resume", map[string]any{
			"threadId": list.Data[0].ID, "excludeTurns": true,
			"developerInstructions": nilIfEmpty(d.DeveloperInstructions),
		}, &res)
		if err == nil {
			return res.Thread.ID, nil
		}
		d.Log.Warn("resuming ore thread failed; starting a new one", "thread", list.Data[0].ID, "err", err)
	}
	err = c.Call(callCtx, "thread/start", map[string]any{
		"cwd": d.WorkDir, "developerInstructions": nilIfEmpty(d.DeveloperInstructions),
	}, &res)
	if err != nil {
		return "", fmt.Errorf("thread/start: %w", err)
	}
	return res.Thread.ID, nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// runTUI keeps `ore resume <thread> --remote` running in tmux.
func (d *Driver) runTUI(ctx context.Context, thread string) {
	sess := d.Session
	sess.Env = append(d.env(), "TERM=xterm-256color")
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := sess.Run(ctx, []string{d.Binary, "resume", thread, "--remote", "unix://" + d.Socket})
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		d.Log.Info("ore TUI exited; restarting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// Deliver implements agent.Agent: turn/steer while a turn runs, else
// turn/start (also when a steer raced the end of the turn).
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
	c, thread, active := d.client, d.thread, d.activeTurn
	d.mu.Unlock()
	if c == nil {
		return errors.New("ore is restarting")
	}
	input := []map[string]any{{"type": "text", "text": text}}
	if active != "" {
		err := c.Call(ctx, "turn/steer", map[string]any{"threadId": thread, "input": input, "expectedTurnId": active}, nil)
		if err == nil {
			return nil
		}
		if !isNoActiveTurn(err) {
			return err
		}
		d.Log.Info("steer raced the end of the turn; starting a new turn", "err", err)
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := c.Call(ctx, "turn/start", map[string]any{"threadId": thread, "input": input}, &res); err != nil {
		return err
	}
	d.mu.Lock()
	if d.activeTurn == "" && res.Turn.ID != "" {
		d.activeTurn = res.Turn.ID
	}
	d.mu.Unlock()
	return nil
}

// Hook implements agent.Agent; ore has no hooks of ours.
func (d *Driver) Hook(context.Context, string, []byte, map[string]string) ([]byte, error) {
	return nil, nil
}

type turnNote struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"turn"`
}

func (d *Driver) onNotification(method string, params json.RawMessage) {
	switch method {
	case "turn/started":
		var n turnNote
		_ = json.Unmarshal(params, &n)
		d.mu.Lock()
		if n.ThreadID == d.thread {
			d.activeTurn = n.Turn.ID
		}
		d.mu.Unlock()
		d.Events.Presence(messagingv1.Presence_PRESENCE_WORKING)
	case "turn/completed":
		var n turnNote
		_ = json.Unmarshal(params, &n)
		d.mu.Lock()
		if n.ThreadID == d.thread && (d.activeTurn == n.Turn.ID || d.activeTurn == "") {
			d.activeTurn = ""
		}
		waiting := d.pendingN > 0
		d.mu.Unlock()
		if !waiting {
			d.Events.Presence(messagingv1.Presence_PRESENCE_IDLE)
		}
		if g, ok := d.Events.(agent.Guardian); ok {
			g.TurnEnded()
		}
	case "item/completed":
		var n struct {
			Item json.RawMessage `json:"item"`
		}
		_ = json.Unmarshal(params, &n)
		if g, ok := d.Events.(agent.Guardian); ok {
			if call, failed, ok := ItemCall(n.Item); ok {
				g.Observe(call, failed)
			}
		}
	case "thread/status/changed":
		var n struct {
			Status struct {
				Type        string   `json:"type"`
				ActiveFlags []string `json:"activeFlags"`
			} `json:"status"`
		}
		_ = json.Unmarshal(params, &n)
		switch {
		case n.Status.Type == "active" && len(n.Status.ActiveFlags) > 0:
			d.Events.Presence(messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN)
		case n.Status.Type == "active":
			d.Events.Presence(messagingv1.Presence_PRESENCE_WORKING)
		case n.Status.Type == "idle":
			d.Events.Presence(messagingv1.Presence_PRESENCE_IDLE)
		}
	case "thread/tokenUsage/updated":
		var n struct {
			TokenUsage struct {
				Total struct{ InputTokens, OutputTokens int64 } `json:"total"`
				Last  struct{ InputTokens, OutputTokens int64 } `json:"last"`
			} `json:"tokenUsage"`
		}
		_ = json.Unmarshal(params, &n)
		u := n.TokenUsage
		d.Events.Tokens(u.Total.InputTokens, u.Total.OutputTokens, u.Last.InputTokens+u.Last.OutputTokens)
	case "serverRequest/resolved":
		var n struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(params, &n)
		d.mu.Lock()
		cancel := d.approvals[string(n.RequestID)]
		d.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case "item/started":
		var n struct {
			Item json.RawMessage `json:"item"`
		}
		_ = json.Unmarshal(params, &n)
		var it struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(n.Item, &it)
		switch it.Type {
		case "fileChange":
			d.mu.Lock()
			d.items[it.ID] = n.Item
			d.mu.Unlock()
		case "userMessage":
			var parts []string
			for _, c := range it.Content {
				parts = append(parts, c.Text)
			}
			if t := strings.TrimSpace(strings.Join(parts, "\n")); t != "" && !strings.HasPrefix(t, "<cross-session-message") {
				d.Events.Task(t)
			}
		}
	}
}

func (d *Driver) onRequest(ctx context.Context, c *Client, id json.RawMessage, method string, params json.RawMessage) {
	var p struct {
		ItemID  string  `json:"itemId"`
		Reason  *string `json:"reason"`
		Command *string `json:"command"`
		Cwd     string  `json:"cwd"`
	}
	_ = json.Unmarshal(params, &p)
	reason := ""
	if p.Reason != nil {
		reason = *p.Reason
	}
	var a agent.Approval
	switch method {
	case "item/commandExecution/requestApproval":
		cmd := ""
		if p.Command != nil {
			cmd = *p.Command
		}
		a = agent.Approval{Tool: "exec", Summary: oneLine(cmd), Detail: string(params), Reason: reason, SessionScope: true}
	case "item/fileChange/requestApproval":
		d.mu.Lock()
		item := d.items[p.ItemID]
		d.mu.Unlock()
		detail := string(params)
		if item != nil {
			detail = string(item)
		}
		a = agent.Approval{Tool: "patch", Summary: oneLine("file change: " + reason), Detail: detail, Reason: reason, SessionScope: true}
	default:
		d.Log.Info("unhandled app-server request; left to the TUI", "method", method)
		return
	}
	if g, ok := d.Events.(agent.Guardian); ok {
		call := agent.ToolCall{Tool: a.Tool, Command: a.Summary, Input: a.Detail}
		if a.Tool == "exec" && p.Command != nil {
			call.Command = *p.Command
		}
		if c2, _, ok := ItemCall(json.RawMessage(a.Detail)); ok && a.Tool == "patch" {
			call = c2
		}
		switch res := g.Guard(ctx, call); res.Action {
		case agent.GuardDeny:
			if err := c.Respond(ctx, id, map[string]any{"decision": "decline"}); err != nil {
				d.Log.Warn("declining for the warden", "err", err)
			}
			return
		case agent.GuardEscalate:
			a.Reason = strings.TrimSpace(a.Reason + "\nEscalated by the warden: " + res.Reason)
		}
	}
	actx, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	d.approvals[string(id)] = cancel
	d.pendingN++
	d.mu.Unlock()
	d.Events.Presence(messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN)
	go func() {
		defer func() {
			cancel()
			d.mu.Lock()
			delete(d.approvals, string(id))
			d.pendingN--
			d.mu.Unlock()
		}()
		dec, err := d.Events.Approve(actx, a)
		if err != nil {
			return // answered in the TUI, or the app-server went away
		}
		decision := "decline"
		switch {
		case dec.Allow && dec.ForSession:
			decision = "acceptForSession"
		case dec.Allow:
			decision = "accept"
		}
		if err := c.Respond(ctx, id, map[string]any{"decision": decision}); err != nil {
			d.Log.Warn("answering approval", "err", err)
		}
		d.Events.Presence(messagingv1.Presence_PRESENCE_WORKING)
	}()
}

func gitTop(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// ItemCall describes a completed thread item for the warden: commands,
// file changes and MCP calls; ok is false for other items.
func ItemCall(item json.RawMessage) (call agent.ToolCall, failed, ok bool) {
	var it struct {
		Type     string `json:"type"`
		Command  string `json:"command"`
		Status   string `json:"status"`
		ExitCode *int   `json:"exitCode"`
		Changes  []struct {
			Path string `json:"path"`
		} `json:"changes"`
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(item, &it) != nil {
		return call, false, false
	}
	failed = it.Status == "failed" || it.Status == "declined" || (it.ExitCode != nil && *it.ExitCode != 0)
	switch it.Type {
	case "commandExecution":
		return agent.ToolCall{Tool: "exec", Command: it.Command}, failed, true
	case "fileChange":
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		return agent.ToolCall{Tool: "patch", Command: strings.Join(paths, " "), Input: string(item)}, failed, true
	case "mcpToolCall":
		return agent.ToolCall{Tool: "mcp__" + it.Server + "__" + it.Tool, Command: string(it.Arguments), Input: string(it.Arguments)}, failed, true
	}
	return call, false, false
}
