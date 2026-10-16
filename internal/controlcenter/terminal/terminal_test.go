package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// echoExec echoes stdin to stdout upper-cased and records the command and resizes.
type echoExec struct {
	mu      sync.Mutex
	cmd     []string
	pod, ns string
	sizes   []Size
}

func (e *echoExec) Exec(ctx context.Context, ns, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan Size) error {
	e.mu.Lock()
	e.cmd, e.pod, e.ns = cmd, pod, ns
	e.mu.Unlock()
	go func() {
		for s := range resize {
			e.mu.Lock()
			e.sizes = append(e.sizes, s)
			e.mu.Unlock()
		}
	}()
	buf := make([]byte, 1024)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if string(buf[:n]) == "exit" {
				return errors.New("command terminated with exit code 1")
			}
			stdout.Write([]byte(strings.ToUpper(string(buf[:n]))))
		}
		if err != nil {
			return nil
		}
	}
}

type pipeFwd struct{ remote io.ReadWriteCloser }

func (p *pipeFwd) Forward(context.Context, string, string, int) (io.ReadWriteCloser, error) {
	return p.remote, nil
}

type rwc struct {
	io.Reader
	io.Writer
}

func (rwc) Close() error { return nil }

// auditLog collects audit events written from the server's goroutines.
type auditLog struct {
	mu  sync.Mutex
	evs []store.AuditEvent
}

func (a *auditLog) add(ev store.AuditEvent) {
	a.mu.Lock()
	a.evs = append(a.evs, ev)
	a.mu.Unlock()
}

// wait returns the events once there are at least n, or after 2 s.
func (a *auditLog) wait(n int) []store.AuditEvent {
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		evs := append([]store.AuditEvent(nil), a.evs...)
		a.mu.Unlock()
		if len(evs) >= n || time.Now().After(deadline) {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func setup(t *testing.T, p *auth.Principal) (*httptest.Server, *echoExec, *auditLog, *pipeFwd) {
	ex := &echoExec{}
	audits := &auditLog{}
	fwd := &pipeFwd{}
	h := &Handler{
		Principal: func(http.ResponseWriter, *http.Request) *auth.Principal { return p },
		Owners: func(_ context.Context, ws string) ([]string, error) {
			if ws != "acme" {
				return nil, errors.New("no")
			}
			return []string{"owner@x"}, nil
		},
		Exec: ex,
		Fwd:  fwd,
		Audit: func(_ context.Context, ev store.AuditEvent) { audits.add(ev) },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/terminal", h.ServeTerminal)
	mux.HandleFunc("/ws/vnc", h.ServeVNC)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, ex, audits, fwd
}

func wsURL(srv *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + path
}

func TestTerminal(t *testing.T) {
	op := &auth.Principal{Role: auth.RoleOperator, Email: "owner@x", Handle: "owner"}
	srv, ex, audits, _ := setup(t, op)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(srv, "/ws/terminal?workspace=acme&room=athena&mode=view"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`))
	c.Write(ctx, websocket.MessageBinary, []byte("hello"))
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(data) != "HELLO" {
		t.Fatalf("read %v %q %v", typ, data, err)
	}
	c.Write(ctx, websocket.MessageBinary, []byte("exit"))
	typ, data, err = c.Read(ctx)
	var m control
	if err != nil || typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m.Type != "exit" || !strings.Contains(m.Error, "exit code 1") {
		t.Fatalf("exit message %v %q %v", typ, data, err)
	}
	c.CloseNow()
	ex.mu.Lock()
	cmd := strings.Join(ex.cmd, " ")
	sizes := ex.sizes
	ex.mu.Unlock()
	if cmd != "runuser -u athena -- env TERM=xterm-256color tmux attach -r -t agent" || ex.pod != "room-athena-0" || ex.ns != "ws-acme" {
		t.Fatalf("exec %q in %s/%s", cmd, ex.ns, ex.pod)
	}
	if len(sizes) != 1 || sizes[0] != (Size{120, 40}) {
		t.Fatalf("resizes %v", sizes)
	}
	if evs := audits.wait(2); len(evs) != 2 || evs[0].Action != "terminal.open" || evs[1].Action != "terminal.close" || !strings.Contains(evs[1].Detail, "durationSeconds") {
		t.Fatalf("audits %+v", evs)
	}
}

func TestTerminalRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		p    *auth.Principal
		q    string
		code int
	}{
		{"anonymous", nil, "workspace=acme&room=athena", 401},
		{"viewer", &auth.Principal{Role: auth.RoleViewer, Workspaces: []string{"acme"}}, "workspace=acme&room=athena", 403},
		{"operator of another workspace", &auth.Principal{Role: auth.RoleOperator, Workspaces: []string{"beta"}}, "workspace=acme&room=athena", 403},
		{"unknown workspace", &auth.Principal{Role: auth.RoleAdmin}, "workspace=nope&room=athena", 404},
		{"bad room", &auth.Principal{Role: auth.RoleAdmin}, "workspace=acme&room=../x", 400},
		{"bad mode", &auth.Principal{Role: auth.RoleAdmin}, "workspace=acme&room=athena&mode=root", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _, _ := setup(t, c.p)
			_, resp, err := websocket.Dial(ctx, wsURL(srv, "/ws/terminal?"+c.q), nil)
			if err == nil || resp == nil || resp.StatusCode != c.code {
				code := 0
				if resp != nil {
					code = resp.StatusCode
				}
				t.Fatalf("got %d (%v), want %d", code, err, c.code)
			}
		})
	}
	// Cross-origin pages can't open a terminal with the person's cookie.
	srv, _, _, _ := setup(t, &auth.Principal{Role: auth.RoleAdmin})
	_, resp, err := websocket.Dial(ctx, wsURL(srv, "/ws/terminal?workspace=acme&room=athena"),
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %v", err)
	}
}

func TestVNC(t *testing.T) {
	srv, _, audits, fwd := setup(t, &auth.Principal{Role: auth.RoleAdmin, Handle: "root"})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go outW.Write([]byte("RFB 003.008\n")) // the room's VNC server greets and stays open
	fwd.remote = rwc{Reader: outR, Writer: inW}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsURL(srv, "/ws/vnc?room=athena@acme"), &websocket.DialOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Sec-WebSocket-Protocol") != "binary" {
		t.Fatal("binary subprotocol not negotiated")
	}
	_, data, err := c.Read(ctx)
	if err != nil || string(data) != "RFB 003.008\n" {
		t.Fatalf("vnc read %q %v", data, err)
	}
	go c.Write(ctx, websocket.MessageBinary, []byte("client"))
	buf := make([]byte, 6)
	if _, err := io.ReadFull(inR, buf); err != nil || string(buf) != "client" {
		t.Fatalf("vnc write %q %v", buf, err)
	}
	c.CloseNow()
	if evs := audits.wait(1); len(evs) == 0 || evs[0].Action != "vnc.open" {
		t.Fatalf("audits %+v", evs)
	}
}
