package warden

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/warden/keytable"
	"github.com/usr-lex/swarmageddon/internal/warden/rules"
	"github.com/usr-lex/swarmageddon/internal/warden/transcript"
)

// Job is a copy of one room request to score.
type Job struct {
	Entry *keytable.Entry
	Path  string
	Body  []byte
}

// Reporter sends verdicts to messaging (ReportVerdict).
type Reporter interface {
	Report(ctx context.Context, v *messagingv1.Verdict) error
}

// Quarantiner sets Room.spec.quarantined.
type Quarantiner interface {
	Quarantine(ctx context.Context, workspace, room, reason string) error
}

// Tuning defaults.
const (
	DefaultReviewEvery = time.Minute
	// firstSightItems is how many trailing items of a room's first request
	// are scored; the rest is history from before this replica started.
	firstSightItems = 12
	seenCap         = 8192
	recentCalls     = 20
	pendingCap      = 200
	repeatWindow    = 10 * time.Minute
	errorReportGap  = 30 * time.Minute
)

// Scorer scores rooms' requests in the background. A slow or failing scorer
// never delays a request: jobs that don't fit the queue are dropped.
type Scorer struct {
	Reporter    Reporter
	Quarantiner Quarantiner
	// Reviewer is the model review; nil means rules only.
	Reviewer Reviewer
	// ReviewEvery is the least time between two model reviews of a room.
	ReviewEvery time.Duration
	// Proxy, if set, refuses a room's requests as soon as it is quarantined.
	Proxy   *Proxy
	Log     *slog.Logger
	Now     func() time.Time
	Workers int

	jobs  chan Job
	start sync.Once
	mu    sync.Mutex
	rooms map[string]*roomState
	// compiled caches each entry's rule table.
	compiled sync.Map // *keytable.Entry -> *rules.Table
}

type roomState struct {
	mu         sync.Mutex
	seen       *transcript.Seen
	task       string
	summary    string
	recent     []string
	pending    []transcript.Item
	calls      []time.Time
	errorsRow  int
	requests   int
	lastReview time.Time
	lastError  time.Time
	reported   map[string]time.Time
}

func (s *Scorer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scorer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Start runs the workers until ctx ends.
func (s *Scorer) Start(ctx context.Context) {
	s.start.Do(func() {
		s.jobs = make(chan Job, 256)
		n := s.Workers
		if n <= 0 {
			n = 4
		}
		for range n {
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case j := <-s.jobs:
						s.Score(ctx, j)
					}
				}
			}()
		}
	})
}

// Enqueue hands a job to the workers without blocking.
func (s *Scorer) Enqueue(j Job) {
	if s.jobs == nil {
		return
	}
	select {
	case s.jobs <- j:
	default:
		s.log().Warn("scoring queue full; request not scored", "room", j.Entry.Room)
	}
}

func (s *Scorer) state(room string) *roomState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rooms == nil {
		s.rooms = map[string]*roomState{}
	}
	st := s.rooms[room]
	if st == nil {
		st = &roomState{seen: transcript.NewSeen(seenCap), reported: map[string]time.Time{}}
		s.rooms[room] = st
	}
	return st
}

func (s *Scorer) table(e *keytable.Entry) *rules.Table {
	if t, ok := s.compiled.Load(e); ok {
		return t.(*rules.Table)
	}
	extra, err := rules.FromSpec(e.Rules)
	if err != nil {
		s.log().Warn("bad persona warden rules; using built-ins", "room", e.Room, "err", err)
	}
	t := rules.Default(extra...)
	s.compiled.Store(e, t)
	return t
}

// Score scores one job synchronously. Workers call it; tests may too.
func (s *Scorer) Score(ctx context.Context, j Job) {
	req, err := transcript.Parse(j.Path, j.Body)
	if err != nil {
		s.log().Debug("not scored", "room", j.Entry.Room, "path", j.Path, "err", err)
		return
	}
	st := s.state(j.Entry.Room)
	st.mu.Lock()
	defer st.mu.Unlock()
	now := s.now()
	st.requests++
	items := req.Items
	if st.task == "" {
		for _, it := range items {
			if it.Kind == transcript.User {
				st.task = clipTo(it.Text, 1000)
				break
			}
		}
	}
	if st.seen.Empty() && len(items) > firstSightItems {
		st.seen.New(items[:len(items)-firstSightItems])
	}
	delta := st.seen.New(items)
	if len(delta) == 0 {
		return
	}

	tab := s.table(j.Entry)
	for _, it := range delta {
		switch it.Kind {
		case transcript.ToolCall:
			st.calls = append(st.calls, now)
			c := rules.NewCall(it.Tool, it.Input)
			st.recent = append(st.recent, clipTo(it.Tool+": "+c.Text, 300))
			if h, ok := tab.Check(c); ok && h.Rule.Action != rules.Allow && h.Rule.Severity >= rules.Medium {
				ev := clipTo(it.Tool+": "+c.Text, 1000)
				dk := h.Rule.Name + "\x00" + ev
				if t, ok := st.reported[dk]; ok && now.Sub(t) < repeatWindow {
					continue
				}
				st.reported[dk] = now
				s.finish(ctx, j.Entry, s.verdict(j.Entry, h.Rule.Severity, h.Rule.Name, h.Rule.Reason, ev))
			}
		case transcript.ToolResult:
			if it.IsError {
				st.errorsRow++
			} else {
				st.errorsRow = 0
			}
		}
	}
	st.recent = tail(st.recent, recentCalls)
	st.calls = since(st.calls, now.Add(-10*time.Minute))
	st.pending = append(st.pending, delta...)
	if len(st.pending) > pendingCap {
		st.pending = st.pending[len(st.pending)-pendingCap:]
	}
	for k, t := range st.reported {
		if now.Sub(t) > repeatWindow {
			delete(st.reported, k)
		}
	}
	s.review(ctx, j.Entry, st, now)
}

// review runs the model review if it is due.
func (s *Scorer) review(ctx context.Context, e *keytable.Entry, st *roomState, now time.Time) {
	every := s.ReviewEvery
	if every <= 0 {
		every = DefaultReviewEvery
	}
	if s.Reviewer == nil || e.Model == "" || len(st.pending) == 0 || now.Sub(st.lastReview) < every {
		return
	}
	st.lastReview = now
	in := ReviewInput{
		Room: e.Room, Task: st.task, Summary: st.summary, Recent: append([]string(nil), st.recent...),
		Delta: st.pending, Rates: Rates{CallsLast10m: len(st.calls), ErrorsInRow: st.errorsRow, Requests: st.requests},
	}
	st.pending = nil
	rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	r, err := s.Reviewer.Review(rctx, e.Model, in)
	if err != nil {
		s.log().Warn("model review failed", "room", e.Room, "err", err)
		// Fail to report: say that the room went unreviewed, at most every errorReportGap.
		if now.Sub(st.lastError) >= errorReportGap {
			st.lastError = now
			v := s.verdict(e, rules.Low, "review-error", "the warden's model review failed: "+clipTo(err.Error(), 300), "")
			v.Action = "unscored"
			s.report(ctx, v)
		}
		return
	}
	if r.Reason != "" {
		st.summary = clipTo(r.Reason, 600)
	}
	if r.Severity < rules.Medium {
		return
	}
	s.finish(ctx, e, s.verdict(e, r.Severity, "model", r.Reason, clipTo(r.Evidence, 1000)))
}

func (s *Scorer) verdict(e *keytable.Entry, sev rules.Severity, rule, reason, evidence string) *messagingv1.Verdict {
	return &messagingv1.Verdict{
		Id: ulid.Make().String(), Room: e.Room, Layer: "gateway", Severity: messagingv1.Severity(sev),
		Rule: rule, Reason: reason, Evidence: evidence, Action: "reported", Source: "gateway",
		At: timestamppb.New(s.now()),
	}
}

// finish quarantines if the verdict calls for it, then reports it.
func (s *Scorer) finish(ctx context.Context, e *keytable.Entry, v *messagingv1.Verdict) {
	sev := rules.Severity(v.Severity)
	already := e.Quarantined || (s.Proxy != nil && s.Proxy.quarantined(e))
	if (sev == rules.Critical || (sev == rules.High && e.QuarantineOnHigh)) && !already {
		if s.Proxy != nil {
			s.Proxy.Hold(e.Room)
		}
		reason := fmt.Sprintf("%s verdict (%s): %s", sev, v.Rule, v.Reason)
		var err error
		if s.Quarantiner != nil {
			for i := range 3 {
				if err = s.Quarantiner.Quarantine(ctx, e.Workspace(), e.Name(), reason); err == nil {
					break
				}
				time.Sleep(time.Duration(i+1) * time.Second)
			}
		}
		if err != nil {
			s.log().Error("quarantine failed", "room", e.Room, "err", err)
			v.Action = "quarantine-failed"
		} else {
			v.Action = "quarantined"
		}
	}
	s.report(ctx, v)
}

func (s *Scorer) report(ctx context.Context, v *messagingv1.Verdict) {
	s.log().Info("verdict", "room", v.Room, "severity", rules.Severity(v.Severity).String(), "rule", v.Rule, "action", v.Action, "reason", v.Reason)
	if s.Reporter == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.Reporter.Report(rctx, v); err != nil {
		s.log().Error("report verdict", "room", v.Room, "err", err)
	}
}

func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func tail[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return append([]T(nil), s[len(s)-n:]...)
}

func since(ts []time.Time, from time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(from) {
		i++
	}
	return ts[i:]
}

// line renders an item for the reviewer.
func line(it transcript.Item) string {
	switch it.Kind {
	case transcript.ToolCall:
		return "[tool call " + it.Tool + "] " + clipTo(rules.Subject(it.Input), 600)
	case transcript.ToolResult:
		tag := "[tool result]"
		if it.IsError {
			tag = "[tool result, error]"
		}
		return tag + " " + clipTo(strings.TrimSpace(it.Text), 400)
	default:
		return "[" + string(it.Kind) + "] " + clipTo(it.Text, 600)
	}
}
