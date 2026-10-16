// Package supervisor is the room supervisor (design §6.3): it connects the
// local CLI driver (agent.Agent) to the messaging service, reports presence
// and status, routes approvals and serves the room-local API for `swarm`.
package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
	"github.com/usr-lex/swarmageddon/internal/supervisor/janitor"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient"
	"github.com/usr-lex/swarmageddon/internal/supervisor/roomcfg"
)

// Volume is a mounted volume whose use is reported.
type Volume struct {
	Name, Path string
}

// Core is the supervisor's runtime state. It implements agent.Events and the
// room-local API.
type Core struct {
	Room      *roomcfg.Room
	CLI       string
	Messaging messagingv1connect.MessagingServiceClient
	Agent     agent.Agent
	Dedup     *msgclient.Dedup
	Volumes   []Volume
	Log       *slog.Logger
	// HeartbeatInterval refreshes presence (default 30 s); StatusInterval
	// reports status even without changes (default 5 min).
	HeartbeatInterval time.Duration
	StatusInterval    time.Duration
	// StatusDebounce is the minimum gap between status reports (default 15 s).
	StatusDebounce time.Duration
	// CoalesceBytes caps one injected message (default 4096).
	CoalesceBytes int

	mu        sync.Mutex
	presence  messagingv1.Presence
	status    messagingv1.RoomStatusReport
	lastExit  time.Time
	approvals map[string]chan *messagingv1.ApprovalAnswer
	presenceC chan struct{}
	statusC   chan struct{}
}

func (c *Core) init() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.approvals == nil {
		c.approvals = map[string]chan *messagingv1.ApprovalAnswer{}
		c.presenceC = make(chan struct{}, 1)
		c.statusC = make(chan struct{}, 1)
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 30 * time.Second
	}
	if c.StatusInterval == 0 {
		c.StatusInterval = 5 * time.Minute
	}
	if c.StatusDebounce == 0 {
		c.StatusDebounce = 15 * time.Second
	}
}

func kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Run delivers messages and reports presence and status until ctx ends. The
// agent itself is run by the caller.
func (c *Core) Run(ctx context.Context) {
	c.init()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); msgclient.Subscribe(ctx, c.Messaging, c.Dedup, c.handleDelivery, c.Log) }()
	go func() { defer wg.Done(); c.presenceLoop(ctx) }()
	go func() { defer wg.Done(); c.statusLoop(ctx) }()
	wg.Wait()
}

// --- agent.Events ---

// Presence implements agent.Events.
func (c *Core) Presence(p messagingv1.Presence) {
	c.init()
	c.mu.Lock()
	changed := c.presence != p
	c.presence = p
	c.mu.Unlock()
	if changed {
		kick(c.presenceC)
	}
}

// CurrentPresence returns the last reported presence.
func (c *Core) CurrentPresence() messagingv1.Presence {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.presence
}

// Task implements agent.Events.
func (c *Core) Task(text string) {
	c.init()
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 300 {
		text = text[:297] + "..."
	}
	c.mu.Lock()
	c.status.CurrentTask = text
	c.mu.Unlock()
	kick(c.statusC)
}

// Tokens implements agent.Events.
func (c *Core) Tokens(input, output, context int64) {
	c.init()
	c.mu.Lock()
	c.status.InputTokens, c.status.OutputTokens, c.status.ContextTokens = input, output, context
	c.mu.Unlock()
	kick(c.statusC)
}

// Session implements agent.Events.
func (c *Core) Session(id string) {
	c.init()
	c.mu.Lock()
	c.status.SessionId = id
	c.mu.Unlock()
	kick(c.statusC)
}

// Exited implements agent.Events: counts consecutive unexpected exits (a
// run of more than 10 minutes resets the count).
func (c *Core) Exited(err error) {
	c.init()
	c.mu.Lock()
	if time.Since(c.lastExit) > 10*time.Minute {
		c.status.CrashCount = 0
	}
	c.status.CrashCount++
	c.lastExit = time.Now()
	n := c.status.CrashCount
	c.presence = messagingv1.Presence_PRESENCE_IDLE
	c.mu.Unlock()
	c.Log.Warn("agent CLI exited", "err", err, "consecutive", n)
	kick(c.statusC)
	kick(c.presenceC)
}

// Approve implements agent.Events: publishes the request and waits for the
// answer delivered through Subscribe. If ctx ends first (answered locally),
// the request is withdrawn.
func (c *Core) Approve(ctx context.Context, a agent.Approval) (agent.Decision, error) {
	c.init()
	id := ulid.Make().String()
	ch := make(chan *messagingv1.ApprovalAnswer, 1)
	c.mu.Lock()
	c.approvals[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.approvals, id)
		c.mu.Unlock()
	}()
	req := &messagingv1.ApprovalRequest{
		Id: id, Room: c.Room.AddressOrDefault(), Cli: c.CLI, Tool: a.Tool, Summary: a.Summary,
		Detail: a.Detail, Reason: a.Reason, SessionScopeAvailable: a.SessionScope,
		RequestedAt: timestamppb.Now(),
	}
	// Publish even if ctx ends meanwhile, so a withdrawal can follow it.
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	_, err := c.Messaging.RequestApproval(pctx, connect.NewRequest(&messagingv1.RequestApprovalRequest{Request: req}))
	pcancel()
	if err != nil {
		c.Log.Warn("approval request not published; it can still be answered in the terminal", "err", err)
		<-ctx.Done()
		return agent.Decision{}, ctx.Err()
	}
	c.Log.Info("approval requested", "id", id, "tool", a.Tool, "summary", a.Summary)
	select {
	case ans := <-ch:
		c.Log.Info("approval answered", "id", id, "decision", ans.GetDecision().String(), "by", ans.GetAnsweredBy())
		return agent.Decision{
			Allow:      ans.GetDecision() == messagingv1.Decision_DECISION_ALLOW || ans.GetDecision() == messagingv1.Decision_DECISION_ALLOW_FOR_SESSION,
			ForSession: ans.GetDecision() == messagingv1.Decision_DECISION_ALLOW_FOR_SESSION,
			Message:    ans.GetMessage(),
		}, nil
	case <-ctx.Done():
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := c.Messaging.WithdrawApproval(wctx, connect.NewRequest(&messagingv1.WithdrawApprovalRequest{ApprovalId: id, Reason: "answered in the terminal"}))
		c.Log.Info("approval withdrawn", "id", id, "err", err)
		return agent.Decision{}, ctx.Err()
	}
}

// --- deliveries ---

func (c *Core) handleDelivery(ctx context.Context, batch []*messagingv1.Delivery) error {
	var items []*messagingv1.Delivery
	for _, d := range batch {
		if a := d.GetApprovalAnswer(); a != nil {
			c.mu.Lock()
			ch := c.approvals[a.GetApprovalId()]
			c.mu.Unlock()
			if ch == nil {
				c.Log.Info("answer for an approval no longer pending", "id", a.GetApprovalId())
				continue
			}
			select {
			case ch <- a:
			default:
			}
			continue
		}
		items = append(items, d)
	}
	if len(items) == 0 {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, d := range items {
		_, id, _ := strings.Cut(msgclient.DeliveryKey(d), ":")
		if id == "" {
			id = d.GetDeliveryId()
		}
		ids = append(ids, id)
	}
	c.Log.Info("delivering to the agent", "items", len(items), "ids", ids)
	return c.Agent.Deliver(ctx, ids[0], Coalesce(items, c.CoalesceBytes))
}

// --- reports ---

func (c *Core) presenceLoop(ctx context.Context) {
	t := time.NewTicker(c.HeartbeatInterval)
	defer t.Stop()
	for {
		p := c.CurrentPresence()
		if p != messagingv1.Presence_PRESENCE_UNSPECIFIED {
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := c.Messaging.ReportPresence(rctx, connect.NewRequest(&messagingv1.ReportPresenceRequest{Presence: p}))
			cancel()
			if err != nil && ctx.Err() == nil {
				c.Log.Debug("presence report failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.presenceC:
		}
	}
}

// StatusReport builds the current status report.
func (c *Core) StatusReport() *messagingv1.RoomStatusReport {
	c.mu.Lock()
	r := &messagingv1.RoomStatusReport{
		CurrentTask: c.status.CurrentTask, InputTokens: c.status.InputTokens, OutputTokens: c.status.OutputTokens,
		ContextTokens: c.status.ContextTokens, CliVersion: c.status.CliVersion, SessionId: c.status.SessionId,
		CrashCount: c.status.CrashCount,
	}
	c.mu.Unlock()
	for _, v := range c.Volumes {
		if u, err := janitor.Stat(v.Path); err == nil {
			r.Volumes = append(r.Volumes, &messagingv1.VolumeUsage{Name: v.Name, UsedBytes: u.Used, CapacityBytes: u.Capacity})
		}
	}
	return r
}

func (c *Core) statusLoop(ctx context.Context) {
	t := time.NewTicker(c.StatusInterval)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := c.Messaging.ReportStatus(rctx, connect.NewRequest(&messagingv1.ReportStatusRequest{Status: c.StatusReport()}))
		cancel()
		if err != nil && ctx.Err() == nil {
			c.Log.Debug("status report failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.statusC:
			// Coalesce bursts (token updates) into one report.
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.StatusDebounce):
			}
		}
	}
}

// SetTask is the room API's set_status.
func (c *Core) SetTask(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("empty status")
	}
	c.Task(text)
	return nil
}
