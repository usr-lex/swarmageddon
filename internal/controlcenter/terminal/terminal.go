// Package terminal bridges browser WebSockets to rooms (docs/design.md §8.4):
//
//   - /ws/terminal?workspace=<ws>&room=<room>&mode=agent|view|shell opens a
//     pods/exec session in the room's container as the agent user: the
//     agent's tmux session (agent), the same read-only (view, tmux attach -r),
//     or a login shell (shell). Binary WebSocket messages carry terminal
//     bytes both ways; text messages carry JSON control messages:
//     {"type":"resize","cols":120,"rows":40} from the browser, and
//     {"type":"exit","error":"..."} from the server before it closes.
//   - /ws/vnc?workspace=<ws>&room=<room> carries the room's VNC server
//     (127.0.0.1:5900 in the room) over pods/portforward, for noVNC
//     (subprotocol "binary").
//
// Both need the operator role on the workspace. Opening, closing and the
// duration are audited; what people type is recorded (terminal.input) only
// in workspaces with spec.recordKeystrokes, and the server then tells the
// browser with {"type":"recording"}. Nothing is ever forwarded into a room:
// VNC flows from the room to the browser only.
package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/redact"
)

// Size is a terminal size.
type Size struct{ Cols, Rows uint16 }

// Execer runs a command in a container with a TTY.
type Execer interface {
	Exec(ctx context.Context, namespace, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan Size) error
}

// Runner runs a command in a container without a TTY or stdin, for
// commands whose output is parsed (transcripts, engine prunes).
type Runner interface {
	Run(ctx context.Context, namespace, pod, container string, cmd []string, stdout, stderr io.Writer) error
}

// Forwarder opens a connection to a port inside a pod.
type Forwarder interface {
	Forward(ctx context.Context, namespace, pod string, port int) (io.ReadWriteCloser, error)
}

// Handler serves both endpoints.
type Handler struct {
	// Principal returns the signed-in person, or nil.
	Principal func(w http.ResponseWriter, r *http.Request) *auth.Principal
	// Owners returns a workspace's spec.owners (and whether it exists).
	Owners func(ctx context.Context, workspace string) ([]string, error)
	Exec   Execer
	Fwd    Forwarder
	Audit  func(ctx context.Context, ev store.AuditEvent)
	// RecordInput reports whether a workspace records keystrokes; nil: none do.
	RecordInput func(ctx context.Context, workspace string) bool
	// OriginPatterns allowed for the WebSocket handshake; empty means same host only.
	OriginPatterns []string
	Container      string // default "room"
	VNCPort        int    // default 5900
}

var roomRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Commands for each terminal mode.
func Command(mode, room string) ([]string, error) {
	switch mode {
	case "agent", "":
		return []string{"runuser", "-u", room, "--", "env", "TERM=xterm-256color", "sh", "-c", attachScript(false), "attach", "1"}, nil
	case "view":
		return []string{"runuser", "-u", room, "--", "env", "TERM=xterm-256color", "sh", "-c", attachScript(true), "attach", "1"}, nil
	case "shell":
		return []string{"env", "TERM=xterm-256color", "runuser", "-l", room}, nil
	}
	return nil, fmt.Errorf("unknown mode %q", mode)
}

// attachScript attaches to the agent's tmux session, waiting up to 30 tries
// ($1 seconds apart) for it to exist: the supervisor creates it only once
// the CLI starts, a few seconds into a room's start.
func attachScript(readOnly bool) string {
	ro := ""
	if readOnly {
		ro = "-r "
	}
	return `i=0
until tmux has-session -t agent 2>/dev/null; do
  i=$((i+1))
  if [ "$i" -ge 30 ]; then echo "The agent's session isn't running. Is the room starting or stopped?"; exit 1; fi
  [ "$i" -eq 1 ] && echo "Waiting for the agent's session to start..."
  sleep "$1"
done
exec tmux attach ` + ro + `-t agent`
}

// authorize checks the request and returns the principal, workspace and room.
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request) (*auth.Principal, string, string, bool) {
	p := h.Principal(w, r)
	if p == nil {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return nil, "", "", false
	}
	ws, room := r.URL.Query().Get("workspace"), r.URL.Query().Get("room")
	if ws == "" {
		if n, w2, ok := cut(room); ok {
			room, ws = n, w2
		}
	}
	if !roomRE.MatchString(ws) || !roomRE.MatchString(room) {
		http.Error(w, "workspace and room are required", http.StatusBadRequest)
		return nil, "", "", false
	}
	owners, err := h.Owners(r.Context(), ws)
	if err != nil {
		http.Error(w, "no such workspace", http.StatusNotFound)
		return nil, "", "", false
	}
	if !p.CanOperate(ws, owners) {
		h.audit(r, p, "terminal.refuse", ws, room, "forbidden", "")
		http.Error(w, "terminals need the operator role on this workspace", http.StatusForbidden)
		return nil, "", "", false
	}
	return p, ws, room, true
}

func cut(addr string) (string, string, bool) {
	for i := 0; i < len(addr); i++ {
		if addr[i] == '@' {
			return addr[:i], addr[i+1:], true
		}
	}
	return "", "", false
}

func (h *Handler) audit(r *http.Request, p *auth.Principal, action, ws, room, result, detail string) {
	if h.Audit == nil {
		return
	}
	h.Audit(r.Context(), store.AuditEvent{Actor: p.Name(), Action: action, Object: "room/" + room, Workspace: ws,
		SourceIP: clientIP(r), Result: result, Detail: detail})
}

func clientIP(r *http.Request) string { return r.RemoteAddr }

func (h *Handler) container() string {
	if h.Container != "" {
		return h.Container
	}
	return "room"
}

type control struct {
	Type  string `json:"type"`
	Cols  uint16 `json:"cols,omitempty"`
	Rows  uint16 `json:"rows,omitempty"`
	Error string `json:"error,omitempty"`
}

// ServeTerminal handles /ws/terminal.
func (h *Handler) ServeTerminal(w http.ResponseWriter, r *http.Request) {
	p, ws, room, ok := h.authorize(w, r)
	if !ok {
		return
	}
	mode := r.URL.Query().Get("mode")
	cmd, err := Command(mode, room)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if mode == "" {
		mode = "agent"
	}
	record := mode != "view" && h.RecordInput != nil && h.RecordInput(r.Context(), ws)
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.OriginPatterns})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	start := time.Now()
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	var rec *inputRecorder
	if record {
		h.audit(r, p, "terminal.open", ws, room, "ok", fmt.Sprintf(`{"mode":%q,"recording":true}`, mode))
		rec = newInputRecorder(func(input string) {
			d, _ := json.Marshal(map[string]string{"mode": mode, "input": redact.Default.String(input)})
			h.audit(r, p, "terminal.input", ws, room, "ok", string(d))
		})
		if b, err := json.Marshal(control{Type: "recording"}); err == nil {
			_ = c.Write(ctx, websocket.MessageText, b)
		}
	} else {
		h.audit(r, p, "terminal.open", ws, room, "ok", fmt.Sprintf(`{"mode":%q}`, mode))
	}

	stdinR, stdinW := io.Pipe()
	resize := make(chan Size, 4)
	// Browser → room.
	go func() {
		defer func() { _ = stdinW.Close() }()
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if rec != nil {
					rec.write(data)
				}
				if _, err := stdinW.Write(data); err != nil {
					return
				}
				continue
			}
			var m control
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" && m.Cols > 0 && m.Rows > 0 {
				select {
				case resize <- Size{Cols: m.Cols, Rows: m.Rows}:
				default:
				}
			}
		}
	}()
	out := &wsWriter{ctx: ctx, c: c}
	err = h.Exec.Exec(ctx, v1alpha1.WorkspaceNamespace(ws), k8s.PodName(room), h.container(), cmd, stdinR, out, resize)
	result := "ok"
	exit := control{Type: "exit"}
	if err != nil && !errors.Is(err, context.Canceled) {
		result = err.Error()
		exit.Error = err.Error()
	}
	if b, merr := json.Marshal(exit); merr == nil {
		wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Write(wctx, websocket.MessageText, b)
		wcancel()
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	if rec != nil {
		cancel() // the reader stops writing to rec
		rec.Close()
	}
	h.audit(r, p, "terminal.close", ws, room, result, fmt.Sprintf(`{"mode":%q,"durationSeconds":%d}`, mode, int(time.Since(start).Seconds())))
}

// wsWriter writes terminal output as binary messages.
type wsWriter struct {
	ctx context.Context
	c   *websocket.Conn
	mu  sync.Mutex
}

func (w *wsWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.c.Write(w.ctx, websocket.MessageBinary, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

// ServeVNC handles /ws/vnc.
func (h *Handler) ServeVNC(w http.ResponseWriter, r *http.Request) {
	p, ws, room, ok := h.authorize(w, r)
	if !ok {
		return
	}
	port := h.VNCPort
	if port == 0 {
		port = 5900
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	remote, err := h.Fwd.Forward(ctx, v1alpha1.WorkspaceNamespace(ws), k8s.PodName(room), port)
	if err != nil {
		h.audit(r, p, "vnc.open", ws, room, err.Error(), "")
		http.Error(w, "can't reach the room's display: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer remote.Close()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.OriginPatterns, Subprotocols: []string{"binary"}})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	start := time.Now()
	h.audit(r, p, "vnc.open", ws, room, "ok", "")
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, nc); done <- struct{}{} }()
	go func() { _, _ = io.Copy(nc, remote); done <- struct{}{} }()
	<-done
	cancel()
	_ = c.Close(websocket.StatusNormalClosure, "")
	h.audit(r, p, "vnc.close", ws, room, "ok", fmt.Sprintf(`{"durationSeconds":%d}`, int(time.Since(start).Seconds())))
}
