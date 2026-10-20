package observatory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/msgclient"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store/storetest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Stop()
	os.Exit(code)
}

func openDB(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// fakeMessaging serves QueryAudit and a status inbox.
type fakeMessaging struct {
	messagingv1connect.UnimplementedMessagingServiceHandler
	records []*messagingv1.AuditRecord
	inbox   []*messagingv1.Delivery
	acked   []string
	asWho   []string
}

func (f *fakeMessaging) QueryAudit(_ context.Context, r *connect.Request[messagingv1.QueryAuditRequest], s *connect.ServerStream[messagingv1.AuditRecord]) error {
	for _, rec := range f.records {
		if !rec.At.AsTime().Before(r.Msg.Since.AsTime()) {
			if err := s.Send(rec); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeMessaging) Subscribe(_ context.Context, r *connect.Request[messagingv1.SubscribeRequest], s *connect.ServerStream[messagingv1.Delivery]) error {
	f.asWho = append(f.asWho, r.Header().Get(msgclient.OnBehalfHeader))
	for _, d := range f.inbox {
		if err := s.Send(d); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeMessaging) Ack(_ context.Context, r *connect.Request[messagingv1.AckRequest]) (*connect.Response[messagingv1.AckResponse], error) {
	f.acked = append(f.acked, r.Msg.DeliveryId)
	return connect.NewResponse(&messagingv1.AckResponse{}), nil
}

func client(t *testing.T, f *fakeMessaging) *msgclient.Client {
	_, h := messagingv1connect.NewMessagingServiceHandler(f)
	mux := http.NewServeMux()
	mux.Handle("/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return msgclient.NewWith(messagingv1connect.NewMessagingServiceClient(srv.Client(), srv.URL))
}

func TestObservatory(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	rec := &Recorder{DB: func() *store.Store { return db }}
	live, cancel := rec.Subscribe("acme")
	defer cancel()

	day := func(d int, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
	rec.Record(ctx,
		store.Activity{TS: day(10, 9), Workspace: "acme", Actor: "athena", Kind: store.KindPROpened, Repo: "acme/api", Number: 2, Summary: "Retry uploads", RefURL: "u2", Dedup: "pr_opened/acme/api/2"},
		store.Activity{TS: day(10, 9), Workspace: "acme", Actor: "athena", Kind: store.KindPROpened, Repo: "acme/api", Number: 2, Dedup: "pr_opened/acme/api/2"}, // replay
		store.Activity{TS: day(10, 10), Workspace: "acme", Actor: "athena", Kind: store.KindReviewRequested, Target: "hermes", Repo: "acme/api", Number: 2},
		store.Activity{TS: day(10, 11), Workspace: "acme", Actor: "hermes", Kind: store.KindReviewChanges, Target: "athena", Repo: "acme/api", Number: 2},
		store.Activity{TS: day(10, 13), Workspace: "acme", Actor: "hermes", Kind: store.KindReviewApproved, Target: "athena", Repo: "acme/api", Number: 2},
		store.Activity{TS: day(10, 14), Workspace: "acme", Actor: "hermes", Kind: store.KindPRMerged, Target: "athena", Repo: "acme/api", Number: 2},
		store.Activity{TS: day(11, 9), Workspace: "acme", Actor: "metis", Kind: store.KindPROpened, Repo: "acme/web", Number: 5, Summary: "Dark mode"},
		store.Activity{TS: day(11, 13), Workspace: "acme", Actor: "athena", Kind: store.KindReviewComment, Target: "metis", Repo: "acme/web", Number: 5},
		store.Activity{TS: day(11, 9), Workspace: "acme", Actor: "athena", Kind: store.KindCommit, Repo: "acme/api", Dedup: "commit/acme/api/abc"},
		store.Activity{TS: day(11, 9), Workspace: "acme", Actor: "athena", Kind: store.KindCommit, Repo: "acme/api", Dedup: "commit/acme/api/abc"},
		store.Activity{TS: day(11, 10), Workspace: "acme", Actor: "athena", Kind: store.KindCIFailed, Repo: "acme/api", Number: 2},
		store.Activity{TS: day(11, 10), Workspace: "other", Actor: "zeus", Kind: store.KindCommit},
	)
	if n := len(live); n != 9 {
		t.Fatalf("live events %d, want 9 (two replays dropped, other workspace filtered)", n)
	}

	// Presence, task and token deltas.
	pr := &Presence{R: rec}
	st := func(task string, in, out int64) *messagingv1.RoomStatusReport {
		return &messagingv1.RoomStatusReport{CurrentTask: task, InputTokens: in, OutputTokens: out}
	}
	pr.Event(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_WORKING, day(11, 15), st("fix flaky test", 100, 50))
	pr.Event(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_WORKING, day(11, 16), st("fix flaky test", 1100, 150))
	pr.Event(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN, day(11, 17), st("fix flaky test", 1100, 150))

	// Messaging audit: an agent message, a person's message, a crossing (also
	// reported as a message), a refusal. Bodies are ignored.
	fm := &fakeMessaging{records: []*messagingv1.AuditRecord{
		{Kind: "message", At: timestamppb.New(day(11, 12)), From: "athena@acme", To: "hermes@acme", MessageId: "m1", Body: "secret"},
		{Kind: "message", At: timestamppb.New(day(11, 12)), From: "alice@acme", To: "athena@acme", MessageId: "m2", Body: "secret"},
		{Kind: "message", At: timestamppb.New(day(11, 12)), From: "hermes@acme", To: "metis@acme", MessageId: "m3", Body: "secret"},
		{Kind: "crossing", At: timestamppb.New(day(11, 12)), From: "hermes@acme", To: "metis@acme", MessageId: "m3", Link: "clean->dirty"},
		{Kind: "refusal", At: timestamppb.New(day(11, 12)), From: "metis@acme", To: "athena@acme", MessageId: "m4", Reason: "only hermes may message"},
		{Kind: "message", At: timestamppb.New(day(11, 12)), From: "athena@acme", To: "#general", MessageId: "p1", Body: "secret"},
		{Kind: "verdict", At: timestamppb.New(day(11, 12)), From: "metis@acme", MessageId: "v1",
			Body: `{"id":"v1","room":"metis@acme","layer":"trajectory","severity":"SEVERITY_HIGH","reason":"looping","action":"gated"}`},
	}}
	rooms := map[string]bool{"athena": true, "hermes": true, "metis": true}
	var verdicts []*messagingv1.Verdict
	f := &AuditFollower{OnVerdict: func(_ context.Context, ws string, v *messagingv1.Verdict) { verdicts = append(verdicts, v) }, Msg: client(t, fm), R: rec, Backfill: 365 * 24 * time.Hour, IsRoom: func(_ context.Context, _, n string) bool { return rooms[n] }}
	if err := f.Poll(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := f.Poll(ctx, "acme"); err != nil { // replay from the cursor: no duplicates
		t.Fatal(err)
	}

	if len(verdicts) != 1 || verdicts[0].Severity != messagingv1.Severity_SEVERITY_HIGH {
		t.Fatalf("verdicts %+v", verdicts)
	}
	if vt, _ := db.Timeline(ctx, "acme", store.TimelineFilter{Kinds: []string{store.KindVerdict}}); len(vt) != 1 || vt[0].Summary != "high trajectory: looping" {
		t.Fatalf("verdict on timeline %+v", vt)
	}
	edges, err := db.Edges(ctx, "acme", day(1, 0), day(30, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"athena>hermes/message": 1, "alice>athena/human": 1, "athena>#general/post": 1, "hermes>metis/crossing": 1, "athena>hermes/review_requested": 1,
	}
	if len(edges) != len(want) {
		t.Fatalf("edges %+v", edges)
	}
	for _, e := range edges {
		if want[e.From+">"+e.To+"/"+e.Kind] != e.Count {
			t.Errorf("edge %+v unexpected", e)
		}
	}

	now, err := db.Now(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	var athena store.NowRow
	for _, r := range now {
		if r.Actor == "athena" {
			athena = r
		}
	}
	if athena.Presence != "waitingOnHuman" || !athena.PresenceSince.Equal(day(11, 17)) || athena.Task != "fix flaky test" ||
		athena.ItemRepo != "acme/web" || athena.ItemNumber != 5 {
		t.Fatalf("now athena %+v", athena)
	}

	heat, _ := db.Heatmap(ctx, "acme", "athena", day(1, 0))
	if len(heat) != 2 || heat[0].Count != 2 || heat[1].Count != 5 {
		t.Fatalf("heatmap %+v", heat)
	}

	axes, _ := db.Axes(ctx, "acme", day(1, 0))
	ax := map[string]store.AxesRow{}
	for _, a := range axes {
		ax[a.Actor] = a
	}
	if a := ax["athena"]; a.Commits != 1 || a.PullRequests != 1 || a.Reviews != 1 || a.Messages != 2 {
		t.Fatalf("axes athena %+v", a)
	}
	if a := ax["hermes"]; a.Reviews != 2 || a.Messages != 1 {
		t.Fatalf("axes hermes %+v", a)
	}

	tl, _ := db.Timeline(ctx, "acme", store.TimelineFilter{Kinds: []string{store.KindCommit, store.KindCIFailed}, Limit: 1})
	if len(tl) != 1 || tl[0].Kind != store.KindCIFailed {
		t.Fatalf("timeline page 1 %+v", tl)
	}
	tl2, _ := db.Timeline(ctx, "acme", store.TimelineFilter{Kinds: []string{store.KindCommit, store.KindCIFailed}, BeforeID: tl[0].ID})
	if len(tl2) != 1 || tl2[0].Kind != store.KindCommit {
		t.Fatalf("timeline page 2 %+v", tl2)
	}

	tp, lat, err := db.Throughput(ctx, "acme", day(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	var merged, ci, tokens int64
	for _, r := range tp {
		if r.Actor == "athena" {
			merged += r.Merged
			ci += r.CIFailures
			tokens += r.Tokens
		}
	}
	if merged != 1 || ci != 1 || tokens != 1100 {
		t.Fatalf("throughput athena merged=%d ci=%d tokens=%d (%+v)", merged, ci, tokens, tp)
	}
	// PR 2: opened 09:00, first review 11:00 (2 h); PR 5: opened 09:00, review 13:00 (4 h).
	if len(lat) != 2 || lat[0].MedianSeconds != 7200 || lat[1].MedianSeconds != 14400 {
		t.Fatalf("latency %+v", lat)
	}

	// Reports from status@acme.
	fm.inbox = []*messagingv1.Delivery{{DeliveryId: "d1", Item: &messagingv1.Delivery_Message{Message: &messagingv1.Message{
		Id: "r1", From: "clio@acme", To: "status@acme", Body: "# Status\nAll good.", SentAt: timestamppb.New(day(11, 18))}}}}
	reps := &Reports{Msg: client(t, fm), DB: func() *store.Store { return db }}
	if err := reps.Follow(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := reps.Follow(ctx, "acme"); err != nil { // redelivery
		t.Fatal(err)
	}
	list, _ := db.Reports(ctx, "acme", 10)
	if len(list) != 1 || list[0].Sender != "clio@acme" || len(fm.acked) != 2 || fm.asWho[0] != "status@acme" {
		t.Fatalf("reports %+v acked %v as %v", list, fm.acked, fm.asWho)
	}

	// Retention.
	if n, _ := db.DeleteActivityBefore(ctx, "acme", day(11, 0)); n != 5 {
		t.Fatalf("retention removed %d", n)
	}
}

// A record that fails to store (in CI run 32 most likely the database going
// away for a moment; here a trigger refusing it) costs neither the records
// after it in the same poll nor, through the cursor, itself: the next poll
// reads it again. Text PostgreSQL can't store (NUL) is
// cleaned instead of failing every poll.
func TestAuditFollowerKeepsTheCursorOnInsertErrors(t *testing.T) {
	url := storetest.URL(t)
	ctx := context.Background()
	db, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	sql, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sql.Close(ctx) }()
	if _, err := sql.Exec(ctx, `CREATE FUNCTION refuse_m2() RETURNS trigger AS $$
		BEGIN IF NEW.dedup = 'audit/refusal/m2/2026-10-20T00:02:00Z' THEN RAISE EXCEPTION 'database went away'; END IF; RETURN NEW; END $$ LANGUAGE plpgsql;
		CREATE TRIGGER refuse_m2 BEFORE INSERT ON activity_events FOR EACH ROW EXECUTE FUNCTION refuse_m2()`); err != nil {
		t.Fatal(err)
	}
	rec := &Recorder{DB: func() *store.Store { return db }}
	at := func(m int) *timestamppb.Timestamp { return timestamppb.New(time.Date(2026, 10, 20, 0, m, 0, 0, time.UTC)) }
	fm := &fakeMessaging{records: []*messagingv1.AuditRecord{
		{Kind: "message", At: at(1), From: "athena@acme", To: "hermes@acme", MessageId: "m1"},
		{Kind: "refusal", At: at(2), From: "athena@acme", To: "metis@acme", MessageId: "m2", Reason: "refused"},
		{Kind: "message", At: at(3), From: "admin@acme", To: "athena@acme", MessageId: "m3"},
		{Kind: "refusal", At: at(4), From: "athena@acme", To: "metis@acme", MessageId: "m4", Reason: "a \x00 byte"},
	}}
	rooms := map[string]bool{"athena": true, "hermes": true, "metis": true}
	f := &AuditFollower{Msg: client(t, fm), R: rec, Backfill: 365 * 24 * time.Hour, IsRoom: func(_ context.Context, _, n string) bool { return rooms[n] }}

	if err := f.Poll(ctx, "acme"); err == nil {
		t.Error("Poll: no error for the record that wasn't stored")
	}
	edges, _ := db.Edges(ctx, "acme", at(0).AsTime(), at(10).AsTime())
	if len(edges) != 2 {
		t.Fatalf("edges %+v, want athena>hermes and admin>athena", edges)
	}
	if cur, _ := db.ActivityCursor(ctx, "acme"); cur.After(at(2).AsTime()) {
		t.Errorf("cursor moved to %v, past the record that failed", cur)
	}

	if _, err := sql.Exec(ctx, `DROP TRIGGER refuse_m2 ON activity_events`); err != nil {
		t.Fatal(err)
	}
	if err := f.Poll(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	tl, _ := db.Timeline(ctx, "acme", store.TimelineFilter{Kinds: []string{store.KindRefusal}})
	if len(tl) != 2 {
		t.Errorf("refusals on the timeline: %d, want 2", len(tl))
	}
	if cur, _ := db.ActivityCursor(ctx, "acme"); !cur.Equal(at(4).AsTime()) {
		t.Errorf("cursor %v, want %v", cur, at(4).AsTime())
	}
}
