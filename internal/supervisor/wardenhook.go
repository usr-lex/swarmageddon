package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/llm"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
	"github.com/usr-lex/swarmageddon/internal/supervisor/warden"
)

// The warden's in-room layers (ADR 0011). Core implements agent.Guardian by
// delegating to Core.Warden; without one, nothing is checked.

// Guard implements agent.Guardian.
func (c *Core) Guard(ctx context.Context, call agent.ToolCall) agent.GuardResult {
	if c.Warden == nil {
		return agent.GuardResult{}
	}
	return c.Warden.Guard(ctx, call)
}

// Observe implements agent.Guardian.
func (c *Core) Observe(call agent.ToolCall, failed bool) {
	if c.Warden != nil {
		c.Warden.Observe(call, failed)
	}
}

// TurnEnded implements agent.Guardian.
func (c *Core) TurnEnded() {
	if c.Warden != nil {
		c.Warden.TurnEnded()
	}
}

var severities = map[string]messagingv1.Severity{
	"low": messagingv1.Severity_SEVERITY_LOW, "medium": messagingv1.Severity_SEVERITY_MEDIUM,
	"high": messagingv1.Severity_SEVERITY_HIGH, "critical": messagingv1.Severity_SEVERITY_CRITICAL,
}

// VerdictProto converts a warden verdict for ReportVerdict (source room).
func VerdictProto(room string, v warden.Verdict) *messagingv1.Verdict {
	return &messagingv1.Verdict{
		Id: ulid.Make().String(), Room: room, Layer: v.Layer, Severity: severities[v.Severity],
		Rule: v.Rule, Reason: v.Reason, Evidence: v.Evidence, Action: v.Action, Source: "room", At: timestamppb.Now(),
	}
}

// reportVerdict sends a verdict to messaging in the background.
func (c *Core) reportVerdict(v warden.Verdict) {
	c.init()
	pv := VerdictProto(c.Room.AddressOrDefault(), v)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.Messaging.ReportVerdict(ctx, connect.NewRequest(&messagingv1.ReportVerdictRequest{Verdict: pv})); err != nil {
			c.Log.Warn("verdict not reported", "err", err, "layer", v.Layer, "rule", v.Rule)
		}
	}()
}

// wardenHigh handles a high trajectory verdict: interrupt the turn if
// configured, and wait on a person.
func (c *Core) wardenHigh(_ warden.Verdict, interrupt bool) {
	if interrupt {
		if in, ok := c.Agent.(agent.Interrupter); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := in.Interrupt(ctx, warden.InterruptNotice); err != nil {
				c.Log.Warn("warden interrupt failed", "err", err)
			}
			cancel()
		}
	}
	c.Presence(messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN)
}

// NewWarden builds the room's warden from room.json, or nil when it is off.
// key is the warden's gateway key; without it or a model, only the rules
// run. Invalid persona rules are dropped, keeping the built-ins.
func NewWarden(room *roomcfg.Room, c *Core, key, roleText, work string) *warden.Warden {
	c.init()
	s := room.Warden
	if s.Mode == warden.ModeOff {
		return nil
	}
	rules, err := warden.Compile(s.Rules)
	if err != nil {
		c.Log.Warn("persona warden rules invalid; using the built-in rules only", "err", err)
		rules, _ = warden.Compile(nil)
	}
	w := &warden.Warden{
		Mode: s.Mode, Rules: rules, InterruptOnHigh: s.InterruptOnHigh, ReviewEvery: s.ReviewEveryToolCalls,
		Report: c.reportVerdict, OnHigh: c.wardenHigh, Log: c.Log.With("component", "warden"),
		Context: func() warden.ReviewContext {
			st := c.StatusReport()
			return warden.ReviewContext{
				Task: st.GetCurrentTask(), Role: roleText, DiffStat: diffStat(work),
				Tokens: fmt.Sprintf("%d in, %d out, context %d", st.GetInputTokens(), st.GetOutputTokens(), st.GetContextTokens()),
			}
		},
	}
	if s.Model != "" && key != "" && room.GatewayURL != "" {
		w.LLM = &llm.Client{BaseURL: room.GatewayURL, Key: key, Model: s.Model, HTTP: nil}
	}
	return w
}

// diffStat summarises uncommitted changes in the work folder.
func diffStat(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-c", "safe.directory=*", "-C", dir, "diff", "--stat", "HEAD").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 15 {
		lines = append(lines[:14], lines[len(lines)-1])
	}
	return strings.Join(lines, "\n")
}
