package server

import (
	"testing"

	"connectrpc.com/connect"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// The control center follows the audit by polling from the newest record's
// time (observatory.AuditFollower); every message must turn up in some poll,
// also after a verdict whose reporter's clock is ahead of the server's.
func TestAuditFollowFromCursor(t *testing.T) {
	reps := startCluster(t, 1, testDirectory())
	cc := clientFor(reps[0], "control-center", "")
	hermes := clientFor(reps[0], "hermes@acme", "")
	proteus := clientFor(reps[0], "proteus@acme", "")
	var cursor time.Time
	seen := map[string]bool{}
	poll := func() {
		q := &messagingv1.QueryAuditRequest{Workspace: "acme", Kinds: []string{"message", "crossing", "refusal", "verdict"}, Limit: 1000}
		if !cursor.IsZero() {
			q.Since = timestamppb.New(cursor)
		}
		recs := queryAudit(t, cc, q)
		for _, r := range recs {
			seen[r.MessageId] = true
			if at := r.At.AsTime(); at.After(cursor) {
				cursor = at
			}
		}
		t.Logf("poll: %d records, cursor %s", len(recs), cursor.Format(time.RFC3339Nano))
	}
	var ids []string
	for i := range 6 {
		id := ulid.Make().String()
		ids = append(ids, id)
		if i%2 == 0 {
			send(t, hermes, id, "proteus", id)
		} else {
			send(t, proteus, id, "hermes", id)
		}
		if i == 1 {
			poll()
			// A room's verdict, dated a little ahead of the server's clock.
			v := &messagingv1.Verdict{Id: ulid.Make().String(), Severity: messagingv1.Severity_SEVERITY_LOW, Layer: "trajectory", Reason: "r",
				At: timestamppb.New(time.Now().Add(2 * time.Second))}
			if _, err := hermes.ReportVerdict(t.Context(), connect.NewRequest(&messagingv1.ReportVerdictRequest{Verdict: v})); err != nil {
				t.Fatal(err)
			}
		}
		if i == 3 {
			poll()
		}
		time.Sleep(300 * time.Millisecond)
	}
	poll()
	for i, id := range ids {
		if !seen[id] {
			t.Errorf("message %d never seen by the follower", i)
		}
	}
}
