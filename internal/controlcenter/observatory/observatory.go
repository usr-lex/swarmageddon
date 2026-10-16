// Package observatory fills and serves the swarm observatory's activity
// table (docs/design.md §8.7). Events come from the messaging audit (message
// metadata only, never bodies), from Forgejo webhooks (forgehook) and from
// presence and status reports. Chronicler reports sent to status@<workspace>
// are stored too.
package observatory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/msgclient"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// Recorder stores events and pushes new ones to live watchers.
type Recorder struct {
	DB  func() *store.Store
	Log *slog.Logger

	mu   sync.Mutex
	subs map[chan store.Activity]string // channel -> workspace
}

// Record stores events and returns the new ones; duplicates (by dedup key)
// are dropped silently.
func (r *Recorder) Record(ctx context.Context, evs ...store.Activity) []store.Activity {
	db := r.DB()
	if db == nil || len(evs) == 0 {
		return nil
	}
	stored, err := db.InsertActivity(ctx, evs...)
	if err != nil {
		r.log().Warn("observatory: insert", "err", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range stored {
		for ch, ws := range r.subs {
			if ws == e.Workspace {
				select {
				case ch <- e:
				default:
				}
			}
		}
	}
	return stored
}

func (r *Recorder) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Subscribe streams new events of a workspace.
func (r *Recorder) Subscribe(ws string) (<-chan store.Activity, func()) {
	ch := make(chan store.Activity, 128)
	r.mu.Lock()
	if r.subs == nil {
		r.subs = map[chan store.Activity]string{}
	}
	r.subs[ch] = ws
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}
}

// ---------------------------------------------------------------- presence

type roomState struct {
	presence string
	task     string
	tokens   int64
	seen     bool
}

// Presence turns presence events into presence, task and token events.
type Presence struct {
	R  *Recorder
	mu sync.Mutex
	st map[string]*roomState
}

// PresenceName is the activity summary for a presence state.
func PresenceName(p messagingv1.Presence) string {
	switch p {
	case messagingv1.Presence_PRESENCE_WORKING:
		return "working"
	case messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN:
		return "waitingOnHuman"
	case messagingv1.Presence_PRESENCE_IDLE:
		return "idle"
	case messagingv1.Presence_PRESENCE_OFFLINE:
		return "offline"
	}
	return ""
}

// Event handles one presence event for room@ws.
func (p *Presence) Event(ctx context.Context, ws, room string, pres messagingv1.Presence, at time.Time, st *messagingv1.RoomStatusReport) {
	p.mu.Lock()
	if p.st == nil {
		p.st = map[string]*roomState{}
	}
	key := ws + "/" + room
	s := p.st[key]
	if s == nil {
		s = &roomState{}
		p.st[key] = s
	}
	var evs []store.Activity
	if name := PresenceName(pres); name != "" && name != s.presence {
		s.presence = name
		evs = append(evs, store.Activity{TS: at, Workspace: ws, Actor: room, Kind: store.KindPresence, Summary: name})
	}
	if st != nil {
		if t := strings.TrimSpace(st.GetCurrentTask()); t != "" && t != s.task {
			s.task = t
			evs = append(evs, store.Activity{TS: at, Workspace: ws, Actor: room, Kind: store.KindTask, Summary: t})
		}
		total := st.GetInputTokens() + st.GetOutputTokens()
		if s.seen && total > s.tokens {
			evs = append(evs, store.Activity{TS: at, Workspace: ws, Actor: room, Kind: store.KindTokens, Value: total - s.tokens})
		}
		if total > 0 || !s.seen {
			s.tokens, s.seen = total, true
		}
	}
	p.mu.Unlock()
	p.R.Record(ctx, evs...)
}

// ---------------------------------------------------------------- messaging audit

// AuditQuerier is the part of the messaging client the follower needs.
type AuditQuerier interface {
	QueryAudit(context.Context, *connect.Request[messagingv1.QueryAuditRequest]) (*connect.ServerStreamForClient[messagingv1.AuditRecord], error)
}

// AuditFollower copies message metadata, crossings and refusals from the
// messaging audit into the activity table, polling each workspace from a
// stored cursor.
type AuditFollower struct {
	Msg        AuditQuerier
	R          *Recorder
	Workspaces func(ctx context.Context) []string
	// IsRoom tells agents from people (and system addresses) apart.
	IsRoom   func(ctx context.Context, ws, name string) bool
	Interval time.Duration
	// OnVerdict is called once per new warden verdict (ADR 0011); optional.
	OnVerdict func(ctx context.Context, ws string, v *messagingv1.Verdict)
	// Backfill is how far back a workspace's first poll reaches; default 24h.
	Backfill time.Duration
}

// Run follows until ctx ends.
func (f *AuditFollower) Run(ctx context.Context) {
	iv := f.Interval
	if iv <= 0 {
		iv = 15 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if f.R.DB() != nil {
			for _, ws := range f.Workspaces(ctx) {
				if err := f.Poll(ctx, ws); err != nil {
					f.R.log().Debug("observatory: audit poll", "workspace", ws, "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll copies one workspace's new audit records.
func (f *AuditFollower) Poll(ctx context.Context, ws string) error {
	db := f.R.DB()
	if db == nil {
		return nil
	}
	since, err := db.ActivityCursor(ctx, ws)
	if err != nil {
		return err
	}
	if since.IsZero() {
		bf := f.Backfill
		if bf <= 0 {
			bf = 24 * time.Hour
		}
		since = time.Now().Add(-bf)
	}
	stream, err := f.Msg.QueryAudit(ctx, connect.NewRequest(&messagingv1.QueryAuditRequest{
		Workspace: ws, Kinds: []string{"message", "crossing", "refusal", "verdict"}, Since: timestamppb.New(since), Limit: 1000,
	}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	latest := since
	var evs []store.Activity
	var crossings []store.Activity
	verdicts := map[string]*messagingv1.Verdict{}
	for stream.Receive() {
		rec := stream.Msg()
		at := rec.GetAt().AsTime()
		if at.After(latest) {
			latest = at
		}
		from, _ := split(rec.GetFrom())
		to, _ := split(rec.GetTo())
		key := "audit/msg/" + rec.GetMessageId()
		switch rec.GetKind() {
		case "message", "post":
			kind := store.KindMessage
			switch {
			case strings.HasPrefix(rec.GetTo(), "#"):
				kind, to = store.KindPost, strings.SplitN(rec.GetTo(), "@", 2)[0]
			case !f.IsRoom(ctx, ws, from) || !f.IsRoom(ctx, ws, to):
				kind = store.KindHuman
			}
			evs = append(evs, store.Activity{TS: at, Workspace: ws, Actor: from, Kind: kind, Target: to, Dedup: key})
		case "verdict":
			var v messagingv1.Verdict
			if err := protojson.Unmarshal([]byte(rec.GetBody()), &v); err != nil || v.GetId() == "" {
				continue
			}
			room, _ := split(v.GetRoom())
			if room == "" {
				room = from
			}
			evs = append(evs, store.Activity{TS: at, Workspace: ws, Actor: room, Kind: store.KindVerdict,
				Summary: fmt.Sprintf("%s %s: %s", severityName(v.GetSeverity()), v.GetLayer(), firstNonEmpty(v.GetReason(), v.GetRule())),
				Value:   int64(v.GetSeverity()), Dedup: "verdict/" + v.GetId()})
			verdicts["verdict/"+v.GetId()] = &v
		case "crossing":
			crossings = append(crossings, store.Activity{TS: at, Workspace: ws, Actor: from, Kind: store.KindCrossing, Target: to,
				Summary: rec.GetLink(), Dedup: key})
		case "refusal":
			evs = append(evs, store.Activity{TS: at, Workspace: ws, Actor: from, Kind: store.KindRefusal, Target: to,
				Summary: rec.GetReason(), Dedup: "audit/refusal/" + rec.GetMessageId() + "/" + at.Format(time.RFC3339Nano)})
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	for _, e := range f.R.Record(ctx, evs...) {
		if v := verdicts[e.Dedup]; v != nil && f.OnVerdict != nil {
			f.OnVerdict(ctx, ws, v)
		}
	}
	// A crossing is also a message: it replaces the plain message event.
	for _, c := range crossings {
		if err := db.MarkCrossing(ctx, c); err != nil {
			return err
		}
	}
	if latest.After(since) {
		return db.SetActivityCursor(ctx, ws, latest)
	}
	return nil
}

func severityName(s messagingv1.Severity) string {
	switch s {
	case messagingv1.Severity_SEVERITY_LOW:
		return "low"
	case messagingv1.Severity_SEVERITY_MEDIUM:
		return "medium"
	case messagingv1.Severity_SEVERITY_HIGH:
		return "high"
	case messagingv1.Severity_SEVERITY_CRITICAL:
		return "critical"
	}
	return "unknown"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func split(addr string) (string, string) {
	n, w, _ := strings.Cut(addr, "@")
	return n, w
}

// ---------------------------------------------------------------- reports

// Subscriber is the part of the messaging client a system inbox needs.
type Subscriber interface {
	Subscribe(context.Context, *connect.Request[messagingv1.SubscribeRequest]) (*connect.ServerStreamForClient[messagingv1.Delivery], error)
	Ack(context.Context, *connect.Request[messagingv1.AckRequest]) (*connect.Response[messagingv1.AckResponse], error)
}

// Reports reads status@<workspace> and stores each message as a report.
type Reports struct {
	Msg Subscriber
	DB  func() *store.Store
	// OnReport is called for each new report (live pages); optional.
	OnReport func(r store.Report)
}

// Follow consumes one workspace's status inbox until the stream ends.
func (r *Reports) Follow(ctx context.Context, ws string) error {
	req := connect.NewRequest(&messagingv1.SubscribeRequest{})
	req.Header().Set(msgclient.OnBehalfHeader, "status@"+ws)
	stream, err := r.Msg.Subscribe(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		d := stream.Msg()
		if m := d.GetMessage(); m != nil {
			db := r.DB()
			if db == nil {
				return nil // leave it unacked until the database is up
			}
			at := time.Now().UTC()
			if m.GetSentAt() != nil {
				at = m.GetSentAt().AsTime()
			}
			rep := store.Report{Workspace: ws, Sender: m.GetFrom(), TS: at, Body: m.GetBody(), MessageID: m.GetId()}
			isNew, err := db.InsertReport(ctx, rep)
			if err != nil {
				return err
			}
			if isNew && r.OnReport != nil {
				r.OnReport(rep)
			}
		}
		ack := connect.NewRequest(&messagingv1.AckRequest{DeliveryId: d.GetDeliveryId()})
		ack.Header().Set(msgclient.OnBehalfHeader, "status@"+ws)
		if _, err := r.Msg.Ack(ctx, ack); err != nil {
			return err
		}
	}
	return stream.Err()
}

// ---------------------------------------------------------------- retention

// RetentionLoop deletes events older than each workspace's audit retention.
func RetentionLoop(ctx context.Context, db func() *store.Store, retention func(ctx context.Context) map[string]time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if d := db(); d != nil {
			for ws, ttl := range retention(ctx) {
				if ttl > 0 {
					_, _ = d.DeleteActivityBefore(ctx, ws, time.Now().Add(-ttl))
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
