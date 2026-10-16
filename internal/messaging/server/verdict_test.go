package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
)

func TestReportVerdict(t *testing.T) {
	reps := startCluster(t, 3, testDirectory())
	ctx := context.Background()
	report := func(id, onBehalf string, v *messagingv1.Verdict) error {
		_, err := clientFor(reps[0], id, onBehalf).ReportVerdict(ctx, connect.NewRequest(&messagingv1.ReportVerdictRequest{Verdict: v}))
		return err
	}

	// A room reports about itself, whatever room it names.
	if err := report("hermes@acme", "", &messagingv1.Verdict{Id: "v1", Room: "metis@acme", Layer: "rules",
		Severity: messagingv1.Severity_SEVERITY_HIGH, Rule: "force-push", Action: "denied"}); err != nil {
		t.Fatal(err)
	}
	// The warden reports about any existing room, and only existing ones.
	if err := report("warden", "", &messagingv1.Verdict{Id: "v2", Room: "metis@acme", Layer: "gateway",
		Severity: messagingv1.Severity_SEVERITY_CRITICAL, Reason: "exfiltration", Action: "quarantined"}); err != nil {
		t.Fatal(err)
	}
	if err := report("warden", "", &messagingv1.Verdict{Id: "v3", Room: "nobody@acme"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown room: %v", err)
	}
	// The control center doesn't report verdicts.
	if err := report("control-center", "", &messagingv1.Verdict{Id: "v4", Room: "metis@acme"}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("control center: %v", err)
	}

	recs := queryAudit(t, clientFor(reps[1], "control-center", ""), &messagingv1.QueryAuditRequest{Workspace: "acme", Kinds: []string{store.AuditVerdict}})
	if len(recs) != 2 {
		t.Fatalf("got %d verdict records, want 2: %v", len(recs), recs)
	}
	got := map[string]*messagingv1.AuditRecord{}
	for _, r := range recs {
		got[r.MessageId] = r
	}
	if r := got["v1"]; r == nil || r.From != "hermes@acme" || !strings.Contains(r.Body, `"source":"room"`) {
		t.Errorf("self-report = %v", r)
	}
	if r := got["v2"]; r == nil || r.From != "metis@acme" || !strings.Contains(r.Body, `"source":"gateway"`) || r.Link != "quarantined" {
		t.Errorf("gateway verdict = %v", r)
	}
}
