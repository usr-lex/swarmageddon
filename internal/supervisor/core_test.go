package supervisor

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient/msgfake"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// fakeAgent records deliveries.
type fakeAgent struct {
	mu        sync.Mutex
	delivered []string
	hook      func(ctx context.Context, event string) ([]byte, error)
}

func (f *fakeAgent) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (f *fakeAgent) Deliver(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, text)
	return nil
}
func (f *fakeAgent) Hook(ctx context.Context, event string, _ []byte, _ map[string]string) ([]byte, error) {
	if f.hook != nil {
		return f.hook(ctx, event)
	}
	return nil, nil
}

// NewTestCore starts a Core against a fake messaging server.
func newTestCore(t *testing.T, a agent.Agent) (*Core, *msgfake.Server, context.Context) {
	t.Helper()
	fake := msgfake.New()
	_, h := messagingv1connect.NewMessagingServiceHandler(fake)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client, _ := msgclient.New(srv.URL, msgclient.TLSFiles{})
	dedup, _ := msgclient.LoadDedup(filepath.Join(t.TempDir(), "seen"), 100)
	c := &Core{
		Room: &roomcfg.Room{Room: "athena", Workspace: "acme", CLI: "claude", RoleTitle: "Reviewer", ForgeURL: "http://forge"},
		CLI:  "claude", Messaging: client, Agent: a, Dedup: dedup,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		StatusDebounce: time.Millisecond,
		Volumes:        []Volume{{Name: "home", Path: t.TempDir()}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go c.Run(ctx)
	return c, fake, ctx
}

func TestCoreDeliversAndReports(t *testing.T) {
	a := &fakeAgent{}
	c, fake, ctx := newTestCore(t, a)
	fake.PushMessage("01M", "hermes@acme", "please review #3")
	c.Presence(messagingv1.Presence_PRESENCE_WORKING)
	c.Task("reviewing #3")
	ok := fake.WaitFor(ctx, func() bool {
		_, acks, presence, status, _, _ := fake.Snapshot()
		return len(acks) == 1 && len(presence) > 0 && presence[len(presence)-1] == messagingv1.Presence_PRESENCE_WORKING &&
			len(status) > 0 && status[len(status)-1].GetCurrentTask() == "reviewing #3"
	})
	if !ok {
		t.Fatal("not delivered/reported")
	}
	a.mu.Lock()
	got := a.delivered[0]
	a.mu.Unlock()
	if !strings.HasPrefix(got, `<cross-session-message from="hermes@acme" from-name="hermes"`) || !strings.Contains(got, "please review #3") {
		t.Errorf("envelope %q", got)
	}
	_, _, _, status, _, _ := fake.Snapshot()
	if len(status[len(status)-1].GetVolumes()) != 1 {
		t.Error("no volume usage")
	}
}

func TestCoreApprovalAnsweredAndWithdrawn(t *testing.T) {
	c, fake, ctx := newTestCore(t, &fakeAgent{})
	fake.Answer = func(r *messagingv1.ApprovalRequest) *messagingv1.ApprovalAnswer {
		if r.GetTool() == "Bash" {
			return &messagingv1.ApprovalAnswer{ApprovalId: r.GetId(), Decision: messagingv1.Decision_DECISION_ALLOW_FOR_SESSION}
		}
		return nil
	}
	dec, err := c.Approve(ctx, agent.Approval{Tool: "Bash", Summary: "Bash: rm x"})
	if err != nil || !dec.Allow || !dec.ForSession {
		t.Fatalf("decision %+v %v", dec, err)
	}
	actx, cancel := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { _, err := c.Approve(actx, agent.Approval{Tool: "Edit"}); errc <- err }()
	fake.WaitFor(ctx, func() bool { _, _, _, _, ap, _ := fake.Snapshot(); return len(ap) == 2 })
	cancel() // answered in the terminal
	if err := <-errc; err == nil {
		t.Error("want cancellation")
	}
	ok := fake.WaitFor(ctx, func() bool { _, _, _, _, _, w := fake.Snapshot(); return len(w) == 1 })
	_, _, _, _, ap, w := fake.Snapshot()
	if !ok || w[0].GetApprovalId() != ap[1].GetId() || ap[0].GetRoom() != "athena@acme" {
		t.Errorf("withdrawn %v approvals %v", w, ap)
	}
}

func TestCrashCount(t *testing.T) {
	c := &Core{Room: &roomcfg.Room{Room: "a", Workspace: "w"}}
	c.Exited(nil)
	c.Exited(nil)
	if c.StatusReport().GetCrashCount() != 2 {
		t.Error("crash count")
	}
}

func TestInstructions(t *testing.T) {
	r := &roomcfg.Room{Room: "athena", Workspace: "acme", CLI: "claude", Group: "clean", RoleTitle: "Reviewer", ForgeURL: "http://forgejo:3000"}
	peers := &messagingv1.ListPeersResponse{Peers: []*messagingv1.Peer{
		{Address: "hermes@acme", Kind: "agent", Title: "Coordinator", Summary: "Plans the work."},
		{Address: "alice@acme", Kind: "human"},
	}}
	s := Instructions(r, "You review pull requests.", peers)
	for _, want := range []string{"# You are Athena", "`athena@acme`, in the group `clean`", "tea pr approve", "send_message",
		"Claude Code's own SendMessage", "## Your role: Reviewer", "You review pull requests.", "- `hermes@acme`: **Coordinator**. Plans the work.", "`alice@acme` (a person)"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	r.CLI = "ore"
	if strings.Contains(Instructions(r, "", nil), "Claude Code's own") {
		t.Error("claude note for ore")
	}
}
