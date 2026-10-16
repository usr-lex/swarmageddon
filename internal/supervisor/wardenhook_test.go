package supervisor

import (
	"context"
	"testing"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
	"github.com/usr-lex/swarmageddon/internal/supervisor/warden"
)

type interruptAgent struct {
	fakeAgent
	notice chan string
}

func (a *interruptAgent) Interrupt(_ context.Context, notice string) error {
	a.notice <- notice
	return nil
}

func TestWardenReportsAndInterrupts(t *testing.T) {
	a := &interruptAgent{notice: make(chan string, 1)}
	c, fake, ctx := newTestCore(t, a)
	c.Room.Warden = roomcfg.WardenSettings{Mode: "monitor", Rules: []roomcfg.WardenRule{{Name: "bad", Match: "(", Action: "deny"}}}
	c.Warden = NewWarden(c.Room, c, "", "", t.TempDir())
	if c.Warden == nil {
		t.Fatal("no warden")
	}
	// Invalid persona rules fall back to the built-ins.
	if r := c.Guard(ctx, agent.ToolCall{Tool: "Bash", Command: "kill -9 1"}); r.Action != agent.GuardDeny {
		t.Errorf("guard %+v", r)
	}
	if !fake.WaitFor(ctx, func() bool { return len(fake.Verdicts()) == 1 }) {
		t.Fatal("no verdict")
	}
	v := fake.Verdicts()[0]
	if v.GetRoom() != "athena@acme" || v.GetLayer() != "rules" || v.GetRule() != "kill-supervisor" || v.GetSource() != "room" ||
		v.GetSeverity() != messagingv1.Severity_SEVERITY_MEDIUM || v.GetAction() != "denied" {
		t.Errorf("verdict %v", v)
	}
	c.wardenHigh(warden.Verdict{}, true)
	if n := <-a.notice; n != warden.InterruptNotice || c.CurrentPresence() != messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN {
		t.Errorf("notice %q presence %v", n, c.CurrentPresence())
	}
	c.Room.Warden.Mode = "off"
	if NewWarden(c.Room, c, "", "", "") != nil {
		t.Error("warden off")
	}
}
