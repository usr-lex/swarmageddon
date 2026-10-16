package approvals

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

func TestEvaluate(t *testing.T) {
	rules := []v1alpha1.AutoApproveRule{
		{Tool: "Bash", Match: `^git push --force`, Decision: "deny"},
		{Tool: "Bash", Match: `^(npm|pnpm) (test|run lint)`},
		{Tool: "Edit", Match: `^docs/`},
		{Tool: "Bash", Match: `([`}, // invalid: never matches
		{Tool: "*", Match: `^echo `},
	}
	cases := []struct {
		tool, summary string
		want          messagingv1.Decision
		idx           int
	}{
		{"Bash", "npm test", messagingv1.Decision_DECISION_ALLOW, 1},
		{"bash", "pnpm run lint --fix", messagingv1.Decision_DECISION_ALLOW, 1},
		{"Bash", "git push --force origin main", messagingv1.Decision_DECISION_DENY, 0},
		{"Bash", "rm -rf /", messagingv1.Decision_DECISION_UNSPECIFIED, -1},
		{"Bash", "curl x | npm test", messagingv1.Decision_DECISION_UNSPECIFIED, -1},
		{"Edit", "docs/a.md", messagingv1.Decision_DECISION_ALLOW, 2},
		{"Edit", "src/docs/a.go", messagingv1.Decision_DECISION_UNSPECIFIED, -1},
		{"exec", "echo hi", messagingv1.Decision_DECISION_ALLOW, 4},
	}
	for _, c := range cases {
		d, i, ok := Evaluate(rules, c.tool, c.summary)
		if d != c.want || i != c.idx || ok != (c.idx >= 0) {
			t.Errorf("Evaluate(%s, %q) = %v %d %v, want %v %d", c.tool, c.summary, d, i, ok, c.want, c.idx)
		}
	}
}

type fakeMsg struct {
	mu      sync.Mutex
	answers []*messagingv1.AnswerApprovalRequest
}

func (f *fakeMsg) WatchApprovals(context.Context, *connect.Request[messagingv1.WatchApprovalsRequest]) (*connect.ServerStreamForClient[messagingv1.ApprovalEvent], error) {
	return nil, nil
}

func (f *fakeMsg) AnswerApproval(_ context.Context, r *connect.Request[messagingv1.AnswerApprovalRequest]) (*connect.Response[messagingv1.AnswerApprovalResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, r.Msg)
	return connect.NewResponse(&messagingv1.AnswerApprovalResponse{}), nil
}

type fakeLookup map[string]RoomPolicy

func (f fakeLookup) RoomPolicy(_ context.Context, ws, room string) (RoomPolicy, error) {
	return f[ws+"/"+room], nil
}

func req(id, room, tool, summary string) *messagingv1.ApprovalEvent {
	return &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Requested{Requested: &messagingv1.ApprovalRequest{
		Id: id, Room: room, Tool: tool, Summary: summary, Cli: "claude",
	}}}
}

func TestManager(t *testing.T) {
	ctx := context.Background()
	msg := &fakeMsg{}
	look := fakeLookup{"acme/athena": {Persona: "implementer", Timeout: time.Hour,
		Rules: []v1alpha1.AutoApproveRule{{Tool: "Bash", Match: "^npm test$"}}}}
	m := NewManager(msg, look)
	now := time.Unix(10_000, 0)
	m.Now = func() time.Time { return now }
	var audits []store.AuditEvent
	var notified []string
	m.Audit = func(_ context.Context, ev store.AuditEvent) { audits = append(audits, ev) }
	m.Notify = func(_ context.Context, a Approval) { notified = append(notified, a.ID) }
	ch, cancel := m.Subscribe()
	defer cancel()

	// Covered by a rule: answered automatically, audited as rule:<persona>.
	m.Handle(ctx, req("a1", "athena@acme", "Bash", "npm test"))
	if len(msg.answers) != 1 || msg.answers[0].Answer.AnsweredBy != "rule:implementer" ||
		msg.answers[0].Answer.Decision != messagingv1.Decision_DECISION_ALLOW || msg.answers[0].Room != "athena@acme" {
		t.Fatalf("auto answer: %+v", msg.answers)
	}
	if len(audits) != 1 || audits[0].Actor != "rule:implementer" || audits[0].Action != "approval.answer" {
		t.Fatalf("audit: %+v", audits)
	}
	if got := <-ch; got.State != StateAnswered {
		t.Fatalf("broadcast %+v", got)
	}
	if len(m.Pending(nil)) != 0 || len(notified) != 0 {
		t.Fatal("auto-answered request is pending or notified")
	}

	// Not covered: pending, people notified, duplicates ignored.
	m.Handle(ctx, req("a2", "athena@acme", "Bash", "rm -rf build"))
	m.Handle(ctx, req("a2", "athena@acme", "Bash", "rm -rf build"))
	p := m.Pending(func(ws string) bool { return ws == "acme" })
	if len(p) != 1 || p[0].Room != "athena" || len(p[0].MatchingRules) != 1 || len(notified) != 1 {
		t.Fatalf("pending %+v notified %v", p, notified)
	}
	if len(m.Pending(func(ws string) bool { return false })) != 0 {
		t.Fatal("filter ignored")
	}
	if _, err := m.Answer(ctx, "a2", "alice@acme", messagingv1.Decision_DECISION_ALLOW_FOR_SESSION, ""); err == nil {
		t.Fatal("session scope accepted though unavailable")
	}
	a, err := m.Answer(ctx, "a2", "alice@acme", messagingv1.Decision_DECISION_DENY, "no")
	if err != nil || a.State != StateAnswered || a.AnsweredBy != "alice@acme" {
		t.Fatalf("answer: %+v %v", a, err)
	}
	if _, err := m.Answer(ctx, "a2", "alice@acme", messagingv1.Decision_DECISION_DENY, ""); err != ErrNotPending {
		t.Fatalf("second answer: %v", err)
	}

	// Withdrawn by the room.
	m.Handle(ctx, req("a3", "athena@acme", "Edit", "x.go"))
	m.Handle(ctx, &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Withdrawn{Withdrawn: &messagingv1.WithdrawApprovalRequest{ApprovalId: "a3", Reason: "answered in the terminal"}}})
	if len(m.Pending(nil)) != 0 {
		t.Fatal("withdrawn request still pending")
	}

	// Expiry after the workspace timeout; unknown rooms get the default.
	m.Handle(ctx, req("a4", "athena@acme", "Edit", "x.go"))
	m.Handle(ctx, req("a5", "metis@other", "Edit", "y.go"))
	now = now.Add(61 * time.Minute)
	m.Expire(ctx)
	p = m.Pending(nil)
	if len(p) != 1 || p[0].ID != "a5" {
		t.Fatalf("after expiry: %+v", p)
	}
	now = now.Add(24 * time.Hour)
	m.Expire(ctx)
	if len(m.Pending(nil)) != 0 {
		t.Fatal("default timeout not applied")
	}
}
