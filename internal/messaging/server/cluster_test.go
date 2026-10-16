package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
)

type replica struct {
	node *store.Node
	st   *store.Store
	http *httptest.Server
	stop context.CancelFunc
}

func (r *replica) close() {
	r.stop()
	r.http.CloseClientConnections()
	r.http.Close()
	r.node.Close()
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func testDirectory() *directory.Static {
	acme := &directory.Workspace{
		Name:            "acme",
		Groups:          []string{"clean", "dirty"},
		ApprovalTimeout: time.Hour,
		Messaging: v1alpha1.MessagingPolicy{
			Links: []v1alpha1.GroupLink{
				{From: "clean", To: "dirty", Mode: v1alpha1.LinkMediated, Senders: []string{"hermes", "proteus"}, Recipients: []string{"metis"}},
				{From: "dirty", To: "clean", Mode: v1alpha1.LinkMediated, Senders: []string{"hecate"}, Recipients: []string{"hermes", "proteus"}, RequirePrefix: "to {recipient}:"},
			},
			Limits: v1alpha1.MessageLimits{RatePerMinute: 1000},
		},
		Rooms: map[string]directory.Room{
			"hermes":  {Name: "hermes", Group: "clean", Title: "Coordinator", Summary: "Plans the work."},
			"proteus": {Name: "proteus", Group: "clean"},
			"themis":  {Name: "themis", Group: "clean"},
			"metis":   {Name: "metis", Group: "dirty"},
			"hecate":  {Name: "hecate", Group: "dirty"},
		},
	}
	fast := &directory.Workspace{
		Name:            "fast",
		ApprovalTimeout: time.Hour,
		Messaging: v1alpha1.MessagingPolicy{
			Retention: v1alpha1.RetentionSettings{Messages: &metav1.Duration{Duration: 4 * time.Second}},
		},
		Rooms: map[string]directory.Room{"a": {Name: "a"}, "b": {Name: "b"}},
	}
	return directory.NewStatic(acme, fast)
}

// startCluster starts n replicas with routes to each other.
func startCluster(t *testing.T, n int, dir directory.Directory) []*replica {
	t.Helper()
	out := io.Discard
	if testing.Verbose() {
		out = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(out, nil))
	ports := make([]int, n)
	var routes []string
	for i := range ports {
		ports[i] = freePort(t)
		routes = append(routes, fmt.Sprintf("nats://127.0.0.1:%d", ports[i]))
	}
	reps := make([]*replica, n)
	for i := range reps {
		node, err := store.StartNode(store.NodeConfig{
			ServerName: fmt.Sprintf("messaging-%d", i), DataDir: t.TempDir(), ClientPort: -1,
			ClusterHost: "127.0.0.1", ClusterPort: ports[i], Routes: routes, ClusterSize: n, Log: log,
		})
		if err != nil {
			t.Fatal(err)
		}
		reps[i] = &replica{node: node}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, r := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.st, errs[i] = store.Open(ctx, r.node, log, store.Options{PresenceTTL: 2 * time.Second})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range reps {
		r.st.AckWait = 5 * time.Second
		srv := New(dir, r.st, log)
		srv.ExpiryInterval, srv.ExpiryMargin = 500*time.Millisecond, 2*time.Second
		rctx, stop := context.WithCancel(context.Background())
		r.stop = stop
		go srv.Run(rctx)
		mux := http.NewServeMux()
		mux.Handle(messagingv1connect.NewMessagingServiceHandler(srv))
		r.http = httptest.NewServer(Identify(mux, true))
	}
	t.Cleanup(func() {
		for _, r := range reps {
			if r != nil {
				r.close()
			}
		}
	})
	return reps
}

type headerTransport struct{ id, onBehalf string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(HeaderDevIdentity, h.id)
	if h.onBehalf != "" {
		r.Header.Set(HeaderOnBehalfOf, h.onBehalf)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func clientFor(r *replica, id, onBehalf string) messagingv1connect.MessagingServiceClient {
	hc := &http.Client{Transport: headerTransport{id, onBehalf}}
	return messagingv1connect.NewMessagingServiceClient(hc, r.http.URL)
}

// inbox reads deliveries from a Subscribe stream in the background.
type inbox struct {
	c      messagingv1connect.MessagingServiceClient
	ch     chan *messagingv1.Delivery
	cancel context.CancelFunc
}

func subscribe(t *testing.T, c messagingv1connect.MessagingServiceClient) *inbox {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.Subscribe(ctx, connect.NewRequest(&messagingv1.SubscribeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	in := &inbox{c: c, ch: make(chan *messagingv1.Delivery, 100), cancel: cancel}
	go func() {
		for stream.Receive() {
			in.ch <- stream.Msg()
		}
		close(in.ch)
	}()
	t.Cleanup(cancel)
	return in
}

func (in *inbox) next(t *testing.T, ack bool) *messagingv1.Delivery {
	t.Helper()
	select {
	case d, ok := <-in.ch:
		if !ok {
			t.Fatal("inbox stream closed")
		}
		if ack {
			if _, err := in.c.Ack(context.Background(), connect.NewRequest(&messagingv1.AckRequest{DeliveryId: d.DeliveryId})); err != nil {
				t.Fatalf("ack: %v", err)
			}
		}
		return d
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for a delivery")
	}
	return nil
}

func (in *inbox) none(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case d := <-in.ch:
		t.Fatalf("unexpected delivery %v", d)
	case <-time.After(wait):
	}
}

func send(t *testing.T, c messagingv1connect.MessagingServiceClient, id, to, body string) *messagingv1.SendResponse {
	t.Helper()
	res, err := c.Send(context.Background(), connect.NewRequest(&messagingv1.SendRequest{Id: id, To: to, Body: body}))
	if err != nil {
		t.Fatalf("send %s to %s: %v", id, to, err)
	}
	return res.Msg
}

func TestCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a 3-replica JetStream cluster")
	}
	reps := startCluster(t, 3, testDirectory())
	ctx := context.Background()
	cc := clientFor(reps[0], ControlCenterCN, "")

	t.Run("dedup and redelivery", func(t *testing.T) {
		hermes := clientFor(reps[0], "hermes@acme", "")
		id := ulid.Make().String()
		if send(t, hermes, id, "themis", "hello").Duplicate {
			t.Fatal("first send is a duplicate")
		}
		if !send(t, clientFor(reps[1], "hermes@acme", ""), id, "themis", "hello").Duplicate {
			t.Fatal("retry not reported as duplicate")
		}
		in := subscribe(t, clientFor(reps[1], "themis@acme", ""))
		d := in.next(t, false)
		if d.GetMessage().GetId() != id || d.GetMessage().GetFrom() != "hermes@acme" || d.GetMessage().GetFromKind() != "agent" {
			t.Fatalf("got %v", d)
		}
		in.cancel() // drop without acking: at-least-once means it comes back
		in2 := subscribe(t, clientFor(reps[2], "themis@acme", ""))
		if d2 := in2.next(t, true); d2.GetMessage().GetId() != id {
			t.Fatalf("redelivered %v", d2)
		}
		in2.none(t, 500*time.Millisecond)
	})

	t.Run("order and offline hold", func(t *testing.T) {
		// proteus is offline while hermes sends through every replica.
		var ids []string
		for i := range 15 {
			id := ulid.Make().String()
			ids = append(ids, id)
			send(t, clientFor(reps[i%3], "hermes@acme", ""), id, "proteus@acme", fmt.Sprint(i))
		}
		in := subscribe(t, clientFor(reps[2], "proteus@acme", ""))
		for i, id := range ids {
			if got := in.next(t, true).GetMessage(); got.GetId() != id {
				t.Fatalf("message %d: got %s (%s); want %s", i, got.GetId(), got.GetBody(), id)
			}
		}
	})

	t.Run("refusal explains and is audited", func(t *testing.T) {
		themis := clientFor(reps[1], "themis@acme", "")
		_, err := themis.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "metis", Body: "x"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "only hermes and proteus may message metis") {
			t.Fatalf("err = %v", err)
		}
		recs := queryAudit(t, cc, &messagingv1.QueryAuditRequest{Workspace: "acme", Kinds: []string{"refusal"}, Address: "themis@acme"})
		if len(recs) != 1 || !strings.Contains(recs[0].Reason, "mediated-sender") {
			t.Fatalf("audit = %v", recs)
		}
		// Rooms can't act for others or call control-center methods.
		if _, err := clientFor(reps[0], "themis@acme", "alice@acme").ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("room on behalf: %v", err)
		}
		if _, err := clientFor(reps[0], "ghost@acme", "").ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("unknown room: %v", err)
		}
	})

	t.Run("crossing, people and forge", func(t *testing.T) {
		hermes := clientFor(reps[0], "hermes@acme", "")
		id := ulid.Make().String()
		send(t, hermes, id, "metis", "please run the replay")
		recs := queryAudit(t, cc, &messagingv1.QueryAuditRequest{Workspace: "acme", Kinds: []string{"crossing"}})
		if len(recs) == 0 || recs[len(recs)-1].MessageId != id || recs[len(recs)-1].Link != "clean->dirty" {
			t.Fatalf("crossing audit = %v", recs)
		}
		metis := subscribe(t, clientFor(reps[1], "metis@acme", ""))
		if m := metis.next(t, true).GetMessage(); m.GetId() != id {
			t.Fatalf("metis got %v", m)
		}
		// A person, through the control center.
		alice := clientFor(reps[2], ControlCenterCN, "alice@acme")
		send(t, alice, ulid.Make().String(), "metis", "status?")
		if m := metis.next(t, true).GetMessage(); m.GetFrom() != "alice@acme" || m.GetFromKind() != "human" {
			t.Fatalf("from alice: %v", m)
		}
		send(t, clientFor(reps[0], "metis@acme", ""), ulid.Make().String(), "alice", "all green")
		ain := subscribe(t, alice)
		if m := ain.next(t, true).GetMessage(); m.GetBody() != "all green" {
			t.Fatalf("alice got %v", m)
		}
		// The forge system sender bypasses links and is one-way.
		forge := clientFor(reps[1], ControlCenterCN, "forge@acme")
		send(t, forge, "forgejo-delivery-1", "metis", `hermes requested your review on acme/api#42 "Retry uploads"`)
		if m := metis.next(t, true).GetMessage(); m.GetFrom() != "forge@acme" || m.GetFromKind() != "system" {
			t.Fatalf("forge notice: %v", m)
		}
		_, err := clientFor(reps[1], "metis@acme", "").Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "forge", Body: "thanks"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "forge notices are one-way; reply on the issue or PR") {
			t.Fatalf("reply to forge: %v", err)
		}
		// status@: any agent reports, only the control center reads.
		send(t, clientFor(reps[2], "hecate@acme", ""), ulid.Make().String(), "status", "shipped #161")
		sin := subscribe(t, clientFor(reps[0], ControlCenterCN, "status@acme"))
		if m := sin.next(t, true).GetMessage(); m.GetFrom() != "hecate@acme" || m.GetTo() != "status@acme" {
			t.Fatalf("status inbox got %v", m)
		}
		if _, err := forge.ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("forge listing peers: %v", err)
		}
		_, err = clientFor(reps[0], ControlCenterCN, "status@acme").Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "metis", Body: "x"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("status sending: %v", err)
		}
		// ListPeers: hecate sees the two contacts with the prefix note, and alice.
		res, err := clientFor(reps[2], "hecate@acme", "").ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, p := range res.Msg.Peers {
			got = append(got, p.Address+"|"+p.Note)
		}
		joined := strings.Join(got, "\n")
		if !strings.Contains(joined, `proteus@acme|mediated link dirty->clean: one named recipient per message, no group addresses; messages must start with "to proteus:"`) ||
			!strings.Contains(joined, "metis@acme|") || !strings.Contains(joined, "status@acme|") || !strings.Contains(joined, "alice@acme|") || strings.Contains(joined, "themis") {
			t.Fatalf("peers:\n%s", joined)
		}
	})

	t.Run("approvals round trip", func(t *testing.T) {
		athena := clientFor(reps[1], "themis@acme", "")
		id := ulid.Make().String()
		if _, err := athena.RequestApproval(ctx, connect.NewRequest(&messagingv1.RequestApprovalRequest{Request: &messagingv1.ApprovalRequest{
			Id: id, Room: "spoofed@acme", Cli: "claude", Tool: "Bash", Summary: "rm -rf build"}})); err != nil {
			t.Fatal(err)
		}
		withdrawn := ulid.Make().String()
		athena.RequestApproval(ctx, connect.NewRequest(&messagingv1.RequestApprovalRequest{Request: &messagingv1.ApprovalRequest{Id: withdrawn, Tool: "Edit"}}))
		athena.WithdrawApproval(ctx, connect.NewRequest(&messagingv1.WithdrawApprovalRequest{ApprovalId: withdrawn, Reason: "answered in the terminal"}))

		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		watch, err := cc.WatchApprovals(wctx, connect.NewRequest(&messagingv1.WatchApprovalsRequest{Workspaces: []string{"acme"}}))
		if err != nil {
			t.Fatal(err)
		}
		events := make(chan *messagingv1.ApprovalEvent, 10)
		go func() {
			for watch.Receive() {
				events <- watch.Msg()
			}
		}()
		ev := <-events // replay: only the pending one
		if ev.GetRequested().GetId() != id || ev.GetRequested().GetRoom() != "themis@acme" {
			t.Fatalf("replayed %v", ev)
		}
		answer := &messagingv1.ApprovalAnswer{ApprovalId: id, Decision: messagingv1.Decision_DECISION_DENY, Message: "not now"}
		alice := clientFor(reps[2], ControlCenterCN, "alice@acme")
		if _, err := alice.AnswerApproval(ctx, connect.NewRequest(&messagingv1.AnswerApprovalRequest{Answer: answer, Room: "themis@acme"})); err != nil {
			t.Fatal(err)
		}
		if _, err := alice.AnswerApproval(ctx, connect.NewRequest(&messagingv1.AnswerApprovalRequest{Answer: answer, Room: "themis@acme"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("second answer: %v", err)
		}
		select {
		case ev := <-events:
			if ev.GetAnswered().GetApprovalId() != id || ev.GetAnswered().GetAnsweredBy() != "alice@acme" {
				t.Fatalf("event %v", ev)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no answered event")
		}
		in := subscribe(t, clientFor(reps[0], "themis@acme", ""))
		if a := in.next(t, true).GetApprovalAnswer(); a.GetApprovalId() != id || a.GetDecision() != messagingv1.Decision_DECISION_DENY {
			t.Fatalf("answer delivery %v", a)
		}
		// Rooms can't answer.
		if _, err := athena.AnswerApproval(ctx, connect.NewRequest(&messagingv1.AnswerApprovalRequest{Answer: answer, Room: "themis@acme"})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("room answering: %v", err)
		}
	})

	t.Run("presence and lapse", func(t *testing.T) {
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		watch, err := cc.WatchPresence(wctx, connect.NewRequest(&messagingv1.WatchPresenceRequest{Workspaces: []string{"fast"}}))
		if err != nil {
			t.Fatal(err)
		}
		events := make(chan *messagingv1.PresenceEvent, 20)
		go func() {
			for watch.Receive() {
				events <- watch.Msg()
			}
			t.Logf("presence watch ended: %v", watch.Err())
		}()
		wait := func(addr string, p messagingv1.Presence, status bool) {
			t.Helper()
			deadline := time.After(15 * time.Second)
			for {
				select {
				case ev := <-events:
					if ev.Address == addr && ev.Presence == p && (!status || ev.Status.GetCurrentTask() != "") {
						return
					}
				case <-deadline:
					t.Fatalf("no %s %s event", addr, p)
				}
			}
		}
		wait("a@fast", messagingv1.Presence_PRESENCE_OFFLINE, false) // known room, never reported
		a := clientFor(reps[1], "a@fast", "")
		if _, err := a.ReportPresence(ctx, connect.NewRequest(&messagingv1.ReportPresenceRequest{Presence: messagingv1.Presence_PRESENCE_WORKING})); err != nil {
			t.Fatal(err)
		}
		wait("a@fast", messagingv1.Presence_PRESENCE_WORKING, false)
		if _, err := a.ReportStatus(ctx, connect.NewRequest(&messagingv1.ReportStatusRequest{Status: &messagingv1.RoomStatusReport{CurrentTask: "#42"}})); err != nil {
			t.Fatal(err)
		}
		wait("a@fast", messagingv1.Presence_PRESENCE_WORKING, true)
		peers, err := clientFor(reps[2], "b@fast", "").ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if p := peers.Msg.Peers[0]; p.Address != "a@fast" || p.CurrentTask != "#42" || p.Presence != messagingv1.Presence_PRESENCE_WORKING {
			t.Fatalf("peer %v", p)
		}
		wait("a@fast", messagingv1.Presence_PRESENCE_OFFLINE, true) // the 2 s TTL lapses
	})

	t.Run("not delivered notice", func(t *testing.T) {
		id := ulid.Make().String()
		send(t, clientFor(reps[0], "a@fast", ""), id, "b", "anyone there?")
		in := subscribe(t, clientFor(reps[2], "a@fast", ""))
		n := in.next(t, true).GetNotice()
		if n.GetAboutMessageId() != id || !strings.Contains(n.GetText(), "Not delivered: b@fast") {
			t.Fatalf("notice %v", n)
		}
		bin := subscribe(t, clientFor(reps[1], "b@fast", ""))
		bin.none(t, time.Second)
	})

	t.Run("replica stopped mid-stream", func(t *testing.T) {
		// Stop the inbox stream's leader; the clients use the other two.
		var leader int
		fmt.Sscanf(reps[0].st.InboxLeader(ctx, "acme"), "messaging-%d", &leader)
		others := []int{(leader + 1) % 3, (leader + 2) % 3}
		in := subscribe(t, clientFor(reps[others[0]], "hermes@acme", ""))
		themis := clientFor(reps[others[1]], "themis@acme", "")
		var ids []string
		for i := range 10 {
			id := ulid.Make().String()
			ids = append(ids, id)
			send(t, themis, id, "hermes", fmt.Sprint(i))
		}
		for _, id := range ids[:5] {
			if got := in.next(t, true).GetMessage().GetId(); got != id {
				t.Fatalf("got %s want %s", got, id)
			}
		}
		reps[leader].close()
		reps[leader] = nil
		for i := range 5 {
			id := ulid.Make().String()
			ids = append(ids, id)
			if err := retrySend(themis, id, "hermes", fmt.Sprint(10+i)); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range ids[5:] {
			d := in.next(t, false)
			for d.GetMessage().GetId() != id && slicesContains(ids[:5], d.GetMessage().GetId()) {
				d = in.next(t, true) // an ack lost to the failover: dedup by id
			}
			if got := d.GetMessage().GetId(); got != id {
				t.Fatalf("got %s want %s", got, id)
			}
			ackRetry(t, in, d)
		}
	})
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// retrySend retries like a supervisor: same id until stored.
func retrySend(c messagingv1connect.MessagingServiceClient, id, to, body string) error {
	var err error
	for range 40 {
		_, err = c.Send(context.Background(), connect.NewRequest(&messagingv1.SendRequest{Id: id, To: to, Body: body}))
		if err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return err
}

func ackRetry(t *testing.T, in *inbox, d *messagingv1.Delivery) {
	t.Helper()
	var err error
	for range 40 {
		_, err = in.c.Ack(context.Background(), connect.NewRequest(&messagingv1.AckRequest{DeliveryId: d.DeliveryId}))
		if err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("ack: %v", err)
}

func queryAudit(t *testing.T, c messagingv1connect.MessagingServiceClient, q *messagingv1.QueryAuditRequest) []*messagingv1.AuditRecord {
	t.Helper()
	stream, err := c.QueryAudit(context.Background(), connect.NewRequest(q))
	if err != nil {
		t.Fatal(err)
	}
	var out []*messagingv1.AuditRecord
	for stream.Receive() {
		out = append(out, stream.Msg())
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return out
}
