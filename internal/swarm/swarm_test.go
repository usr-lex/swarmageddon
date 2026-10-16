package swarm

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	roomv1 "github.com/usr-lex/swarmageddon/gen/swarm/room/v1"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/supervisor"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient/msgfake"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

type hookAgent struct{ events []string }

func (h *hookAgent) Run(ctx context.Context) error                 { <-ctx.Done(); return nil }
func (h *hookAgent) Deliver(context.Context, string, string) error { return nil }
func (h *hookAgent) Hook(_ context.Context, ev string, p []byte, env map[string]string) ([]byte, error) {
	h.events = append(h.events, ev+":"+string(p)+":"+env["CLAUDE_CODE_MESSAGING_SOCKET"])
	if ev == "PermissionRequest" {
		return []byte(`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`), nil
	}
	return nil, nil
}

func setup(t *testing.T) (*msgfake.Server, *hookAgent, string, context.Context) {
	fake := msgfake.New()
	fake.Self = "athena@acme"
	fake.Peers = []*messagingv1.Peer{{Address: "hermes@acme", Kind: "agent", Title: "Coordinator", Presence: messagingv1.Presence_PRESENCE_IDLE}}
	_, h := messagingv1connect.NewMessagingServiceHandler(fake)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	mc, _ := msgclient.New(srv.URL, msgclient.TLSFiles{})
	ag := &hookAgent{}
	core := &supervisor.Core{
		Room:      &roomcfg.Room{Room: "athena", Workspace: "acme", CLI: "claude", RoleTitle: "Reviewer"},
		Messaging: mc, Agent: ag, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	sock := filepath.Join(t.TempDir(), "s.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	api := &supervisor.API{Core: core}
	go supervisor.ServeUnix(ctx, sock, -1, api.Handler())
	for i := 0; i < 100; i++ {
		if _, err := NewClient(sock).WhoAmI(ctx, connect.NewRequest(&roomv1.WhoAmIRequest{})); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fake, ag, sock, ctx
}

func TestCLIAndHook(t *testing.T) {
	fake, ag, sock, ctx := setup(t)
	c := NewClient(sock)
	var out bytes.Buffer
	if err := CLI(ctx, c, []string{"send", "hermes", "hello", "there"}, &out); err != nil {
		t.Fatal(err)
	}
	sent, _, _, _, _, _ := fake.Snapshot()
	if len(sent) != 1 || sent[0].GetTo() != "hermes" || sent[0].GetBody() != "hello there" || sent[0].GetId() == "" {
		t.Errorf("sent %v", sent)
	}
	out.Reset()
	CLI(ctx, c, []string{"agents"}, &out)
	if !strings.Contains(out.String(), "hermes@acme") || !strings.Contains(out.String(), "idle") {
		t.Errorf("agents: %s", out.String())
	}
	out.Reset()
	CLI(ctx, c, []string{"whoami"}, &out)
	if !strings.Contains(out.String(), `"address": "athena@acme"`) {
		t.Errorf("whoami: %s", out.String())
	}
	if err := CLI(ctx, c, []string{"status"}, &out); err == nil {
		t.Error("status without text accepted")
	}

	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/tmp/cc-socks/1.sock")
	var stdout, stderr bytes.Buffer
	Hook(ctx, c, "SessionStart", strings.NewReader(`{"session_id":"s"}`), &stdout, &stderr)
	Hook(ctx, c, "PermissionRequest", strings.NewReader(`{}`), &stdout, &stderr)
	if ag.events[0] != `SessionStart:{"session_id":"s"}:/tmp/cc-socks/1.sock` {
		t.Errorf("events %v", ag.events)
	}
	if !strings.Contains(stdout.String(), `"behavior":"allow"`) || stderr.Len() != 0 {
		t.Errorf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	// A dead supervisor never fails the hook.
	stdout.Reset()
	Hook(ctx, NewClient(filepath.Join(t.TempDir(), "none.sock")), "Stop", strings.NewReader(`{}`), &stdout, &stderr)
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "swarm hook Stop") {
		t.Errorf("dead supervisor: %q %q", stdout.String(), stderr.String())
	}
}

func TestMCPTools(t *testing.T) {
	fake, _, sock, ctx := setup(t)
	server := NewMCPServer(NewClient(sock))
	ct, st := mcp.NewInMemoryTransports()
	go server.Run(ctx, st)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "list_agents,send_message,set_status,whoami" {
		t.Errorf("tools %v", names)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "hermes@acme", "body": "hi"}})
	if err != nil || res.IsError {
		t.Fatalf("send: %v %+v", err, res)
	}
	sent, _, _, _, _, _ := fake.Snapshot()
	if len(sent) != 1 || sent[0].GetBody() != "hi" {
		t.Errorf("sent %v", sent)
	}
	res, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "", "body": "hi"}})
	if !res.IsError {
		t.Error("empty recipient accepted")
	}
	res, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}})
	if !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "hermes@acme") {
		t.Errorf("list: %+v", res.Content[0])
	}
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "set_status", Arguments: map[string]any{"current_task": "testing"}}); err != nil {
		t.Fatal(err)
	}
}
