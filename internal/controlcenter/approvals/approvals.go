// Package approvals keeps the approvals inbox (docs/design.md §8.5). It
// watches approval requests from messaging, answers those an automatic rule
// of the room's persona covers (RoleTemplate.spec.autoApprove, audited as
// rule:<persona>), keeps the rest pending for people, sends people's answers
// back through messaging, and expires requests after the workspace's
// approval timeout (default 24 h). Expiry only removes a request from the
// inbox; the CLI's own dialog stays open in the room.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// DefaultTimeout is how long a request waits when the workspace sets none.
const DefaultTimeout = 24 * time.Hour

// States of an approval.
const (
	StatePending   = "pending"
	StateAnswered  = "answered"
	StateWithdrawn = "withdrawn"
	StateExpired   = "expired"
)

// Evaluate returns the decision of the first rule matching the tool and
// summary (the command or file path), and that rule's index. Tool "*"
// matches any tool; an empty Match matches anything; an invalid regular
// expression never matches.
func Evaluate(rules []v1alpha1.AutoApproveRule, tool, summary string) (messagingv1.Decision, int, bool) {
	for i, r := range rules {
		if r.Tool != "*" && !strings.EqualFold(r.Tool, tool) {
			continue
		}
		if r.Match != "" {
			re, err := regexp.Compile(r.Match)
			if err != nil || !re.MatchString(summary) {
				continue
			}
		}
		if r.Decision == "deny" {
			return messagingv1.Decision_DECISION_DENY, i, true
		}
		return messagingv1.Decision_DECISION_ALLOW, i, true
	}
	return messagingv1.Decision_DECISION_UNSPECIFIED, -1, false
}

// RoomPolicy is what the manager needs to know about a room.
type RoomPolicy struct {
	Persona string // RoleTemplate name
	Rules   []v1alpha1.AutoApproveRule
	Timeout time.Duration
}

// Lookup finds a room's policy.
type Lookup interface {
	RoomPolicy(ctx context.Context, workspace, room string) (RoomPolicy, error)
}

// Messaging is the part of the messaging client approvals use.
type Messaging interface {
	WatchApprovals(context.Context, *connect.Request[messagingv1.WatchApprovalsRequest]) (*connect.ServerStreamForClient[messagingv1.ApprovalEvent], error)
	AnswerApproval(context.Context, *connect.Request[messagingv1.AnswerApprovalRequest]) (*connect.Response[messagingv1.AnswerApprovalResponse], error)
}

// Approval is an approval in the inbox, with its persona's matching rules.
type Approval struct {
	store.Approval
	MatchingRules []string
}

// Manager keeps pending approvals.
type Manager struct {
	Messaging Messaging
	Lookup    Lookup
	// Optional hooks.
	Persist func(ctx context.Context, a store.Approval)
	Audit   func(ctx context.Context, ev store.AuditEvent)
	Notify  func(ctx context.Context, a Approval) // a new request waits for people
	Now     func() time.Time

	mu      sync.Mutex
	pending map[string]*Approval
	subs    map[chan Approval]struct{}
}

// NewManager returns a manager.
func NewManager(m Messaging, l Lookup) *Manager {
	return &Manager{Messaging: m, Lookup: l, Now: time.Now, pending: map[string]*Approval{}, subs: map[chan Approval]struct{}{}}
}

// SplitAddress splits name@workspace.
func SplitAddress(addr string) (name, workspace string) {
	name, workspace, _ = strings.Cut(addr, "@")
	return name, workspace
}

// Watch consumes one WatchApprovals stream until it ends.
func (m *Manager) Watch(ctx context.Context) error {
	stream, err := m.Messaging.WatchApprovals(ctx, connect.NewRequest(&messagingv1.WatchApprovalsRequest{}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		m.Handle(ctx, stream.Msg())
	}
	return stream.Err()
}

// Handle applies one event from messaging.
func (m *Manager) Handle(ctx context.Context, ev *messagingv1.ApprovalEvent) {
	switch e := ev.Event.(type) {
	case *messagingv1.ApprovalEvent_Requested:
		m.requested(ctx, e.Requested)
	case *messagingv1.ApprovalEvent_Answered:
		a := e.Answered
		m.finish(ctx, a.GetApprovalId(), func(ap *Approval) {
			ap.State = StateAnswered
			ap.Decision = decisionName(a.GetDecision())
			ap.AnsweredBy = a.GetAnsweredBy()
			ap.AnswerMessage = a.GetMessage()
			t := m.Now().UTC()
			if a.GetAnsweredAt() != nil {
				t = a.GetAnsweredAt().AsTime()
			}
			ap.AnsweredAt = &t
		})
	case *messagingv1.ApprovalEvent_Withdrawn:
		w := e.Withdrawn
		m.finish(ctx, w.GetApprovalId(), func(ap *Approval) {
			ap.State = StateWithdrawn
			ap.AnswerMessage = w.GetReason()
			t := m.Now().UTC()
			ap.AnsweredAt = &t
		})
	}
}

func (m *Manager) requested(ctx context.Context, r *messagingv1.ApprovalRequest) {
	if r == nil || r.GetId() == "" {
		return
	}
	m.mu.Lock()
	_, seen := m.pending[r.GetId()]
	m.mu.Unlock()
	if seen {
		return
	}
	room, ws := SplitAddress(r.GetRoom())
	now := m.Now().UTC()
	a := &Approval{Approval: store.Approval{
		ID: r.GetId(), Workspace: ws, Room: room, CLI: r.GetCli(), Tool: r.GetTool(), Summary: r.GetSummary(),
		Detail: r.GetDetail(), Reason: r.GetReason(), SessionScopeAvailable: r.GetSessionScopeAvailable(),
		RequestedAt: now, State: StatePending,
	}}
	if r.GetRequestedAt() != nil {
		a.RequestedAt = r.GetRequestedAt().AsTime()
	}
	pol, err := m.Lookup.RoomPolicy(ctx, ws, room)
	if err != nil {
		slog.Warn("approvals: no policy for room; keeping the request for people", "room", r.GetRoom(), "err", err)
	}
	timeout := pol.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	exp := a.RequestedAt.Add(timeout)
	a.ExpiresAt = &exp
	for _, rule := range pol.Rules {
		if rule.Tool == "*" || strings.EqualFold(rule.Tool, a.Tool) {
			a.MatchingRules = append(a.MatchingRules, describeRule(rule))
		}
	}
	if dec, idx, ok := Evaluate(pol.Rules, a.Tool, a.Summary); ok {
		by := "rule:" + pol.Persona
		msg := "automatic rule: " + describeRule(pol.Rules[idx])
		_, err := m.Messaging.AnswerApproval(ctx, connect.NewRequest(&messagingv1.AnswerApprovalRequest{
			Room: r.GetRoom(),
			Answer: &messagingv1.ApprovalAnswer{ApprovalId: a.ID, Decision: dec, Message: msg, AnsweredBy: by,
				AnsweredAt: timestamppb.New(now)},
		}))
		result := "ok"
		if err != nil {
			result = err.Error()
		}
		m.audit(ctx, store.AuditEvent{Actor: by, Action: "approval.answer", Object: "approval/" + a.ID, Workspace: ws,
			Result: result, Detail: fmt.Sprintf(`{"room":%q,"tool":%q,"summary":%q,"decision":%q}`, room, a.Tool, a.Summary, decisionName(dec))})
		if err == nil {
			a.State = StateAnswered
			a.Decision = decisionName(dec)
			a.AnsweredBy = by
			a.AnswerMessage = msg
			a.AnsweredAt = &now
			m.persist(ctx, a.Approval)
			m.broadcast(*a)
			return
		}
		slog.Warn("approvals: automatic answer failed; keeping the request for people", "id", a.ID, "err", err)
	}
	m.mu.Lock()
	m.pending[a.ID] = a
	m.mu.Unlock()
	m.persist(ctx, a.Approval)
	m.broadcast(*a)
	if m.Notify != nil {
		m.Notify(ctx, *a)
	}
}

func (m *Manager) finish(ctx context.Context, id string, mut func(*Approval)) {
	m.mu.Lock()
	a, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	mut(a)
	m.persist(ctx, a.Approval)
	m.broadcast(*a)
}

// ErrNotPending is returned when answering an unknown or settled approval.
var ErrNotPending = errors.New("approval is not pending")

// Get returns a pending approval.
func (m *Manager) Get(id string) (Approval, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.pending[id]
	if !ok {
		return Approval{}, false
	}
	return *a, true
}

// Answer sends a person's answer. by is the person's address (handle@workspace).
func (m *Manager) Answer(ctx context.Context, id, by string, dec messagingv1.Decision, message string) (Approval, error) {
	a, ok := m.Get(id)
	if !ok {
		return Approval{}, ErrNotPending
	}
	if dec == messagingv1.Decision_DECISION_UNSPECIFIED {
		return Approval{}, errors.New("a decision is required")
	}
	if dec == messagingv1.Decision_DECISION_ALLOW_FOR_SESSION && !a.SessionScopeAvailable {
		return Approval{}, errors.New("this request can't be allowed for the session")
	}
	now := m.Now().UTC()
	_, err := m.Messaging.AnswerApproval(ctx, connect.NewRequest(&messagingv1.AnswerApprovalRequest{
		Room:   a.Room + "@" + a.Workspace,
		Answer: &messagingv1.ApprovalAnswer{ApprovalId: id, Decision: dec, Message: message, AnsweredBy: by, AnsweredAt: timestamppb.New(now)},
	}))
	if err != nil {
		return Approval{}, err
	}
	var out Approval
	m.finish(ctx, id, func(ap *Approval) {
		ap.State = StateAnswered
		ap.Decision = decisionName(dec)
		ap.AnsweredBy = by
		ap.AnswerMessage = message
		ap.AnsweredAt = &now
		out = *ap
	})
	return out, nil
}

// Pending returns the pending approvals visible through allow, oldest first.
func (m *Manager) Pending(allow func(workspace string) bool) []Approval {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Approval
	for _, a := range m.pending {
		if allow == nil || allow(a.Workspace) {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.Before(out[j].RequestedAt) })
	return out
}

// PendingCount returns the number of pending approvals per workspace/room.
func (m *Manager) PendingCount() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, a := range m.pending {
		out[a.Workspace+"/"+a.Room]++
	}
	return out
}

// Expire removes requests past their timeout.
func (m *Manager) Expire(ctx context.Context) {
	now := m.Now()
	m.mu.Lock()
	var ids []string
	for id, a := range m.pending {
		if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.finish(ctx, id, func(a *Approval) { a.State = StateExpired })
		m.audit(ctx, store.AuditEvent{Actor: "system", Action: "approval.expire", Object: "approval/" + id})
	}
}

// ExpireLoop runs Expire every minute.
func (m *Manager) ExpireLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Expire(ctx)
		}
	}
}

// Subscribe returns a channel of approval changes and a cancel func.
func (m *Manager) Subscribe() (<-chan Approval, func()) {
	ch := make(chan Approval, 64)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		if _, ok := m.subs[ch]; ok {
			delete(m.subs, ch)
			close(ch)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) broadcast(a Approval) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- a:
		default: // a slow watcher misses an update; it can re-list
		}
	}
}

func (m *Manager) persist(ctx context.Context, a store.Approval) {
	if m.Persist != nil {
		m.Persist(ctx, a)
	}
}

func (m *Manager) audit(ctx context.Context, ev store.AuditEvent) {
	if m.Audit != nil {
		m.Audit(ctx, ev)
	}
}

func describeRule(r v1alpha1.AutoApproveRule) string {
	d := string(r.Decision)
	if d == "" {
		d = "allow"
	}
	if r.Match == "" {
		return fmt.Sprintf("%s %s", d, r.Tool)
	}
	return fmt.Sprintf("%s %s matching %s", d, r.Tool, r.Match)
}

func decisionName(d messagingv1.Decision) string {
	switch d {
	case messagingv1.Decision_DECISION_ALLOW:
		return "allow"
	case messagingv1.Decision_DECISION_ALLOW_FOR_SESSION:
		return "allowForSession"
	case messagingv1.Decision_DECISION_DENY:
		return "deny"
	}
	return ""
}

// DecisionFromName is the inverse of the names stored in history.
func DecisionFromName(s string) messagingv1.Decision {
	switch s {
	case "allow":
		return messagingv1.Decision_DECISION_ALLOW
	case "allowForSession":
		return messagingv1.Decision_DECISION_ALLOW_FOR_SESSION
	case "deny":
		return messagingv1.Decision_DECISION_DENY
	}
	return messagingv1.Decision_DECISION_UNSPECIFIED
}
