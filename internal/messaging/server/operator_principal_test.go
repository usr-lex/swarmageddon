package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

func TestParseCallerOperator(t *testing.T) {
	c, err := parseCaller(OperatorCN, "")
	if err != nil || !c.Operator || c.ControlCenter || c.Name != "" || c.Address() != OperatorCN {
		t.Fatalf("operator caller = %+v, %v", c, err)
	}
	if _, err := parseCaller(OperatorCN, "alice@acme"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("operator acting for someone: %v", err)
	}
}

// TestOperatorPrincipal: the operator may watch presence and approvals, and
// nothing else.
func TestOperatorPrincipal(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster test")
	}
	reps := startCluster(t, 3, testDirectory())
	ctx := context.Background()
	op := clientFor(reps[0], OperatorCN, "")

	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	watch, err := op.WatchPresence(wctx, connect.NewRequest(&messagingv1.WatchPresenceRequest{Workspaces: []string{"fast"}}))
	if err != nil {
		t.Fatal(err)
	}
	a := clientFor(reps[0], "a@fast", "")
	if _, err := a.ReportPresence(ctx, connect.NewRequest(&messagingv1.ReportPresenceRequest{Presence: messagingv1.Presence_PRESENCE_IDLE})); err != nil {
		t.Fatal(err)
	}
	for {
		if !watch.Receive() {
			t.Fatalf("presence watch ended: %v", watch.Err())
		}
		if ev := watch.Msg(); ev.Address == "a@fast" && ev.Presence == messagingv1.Presence_PRESENCE_IDLE {
			break
		}
	}
	appr, err := op.WatchApprovals(wctx, connect.NewRequest(&messagingv1.WatchApprovalsRequest{Workspaces: []string{"fast"}}))
	if err != nil {
		t.Fatal(err)
	}
	_ = appr.Close()

	denied := map[string]error{}
	_, denied["Send"] = op.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "a@fast", Body: "hi"}))
	_, denied["ListPeers"] = op.ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{}))
	_, denied["ReportPresence"] = op.ReportPresence(ctx, connect.NewRequest(&messagingv1.ReportPresenceRequest{Presence: messagingv1.Presence_PRESENCE_IDLE}))
	_, denied["AnswerApproval"] = op.AnswerApproval(ctx, connect.NewRequest(&messagingv1.AnswerApprovalRequest{
		Answer: &messagingv1.ApprovalAnswer{ApprovalId: ulid.Make().String(), Decision: messagingv1.Decision_DECISION_ALLOW}, Room: "a@fast"}))
	if q, err := op.QueryAudit(ctx, connect.NewRequest(&messagingv1.QueryAuditRequest{Workspace: "fast"})); err == nil {
		for q.Receive() {
		}
		denied["QueryAudit"] = q.Err()
	} else {
		denied["QueryAudit"] = err
	}
	for rpc, err := range denied {
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("operator %s: %v, want PermissionDenied", rpc, err)
		}
	}
}
