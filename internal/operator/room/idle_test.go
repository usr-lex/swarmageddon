package room

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	msgserver "github.com/usr-lex/swarmageddon/internal/messaging/server"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func idleEv(addr string, p messagingv1.Presence) *messagingv1.PresenceEvent {
	return &messagingv1.PresenceEvent{Address: addr, Presence: p}
}

func isIdle(t *testing.T, p *PresenceIdle, room string) bool {
	t.Helper()
	ok, err := p.Idle(context.Background(), "acme", room)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestPresenceIdleRules(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	p := &PresenceIdle{Now: clk.Now}
	p.presence(idleEv("athena@acme", messagingv1.Presence_PRESENCE_IDLE))
	clk.Add(time.Hour)
	if isIdle(t, p, "athena") {
		t.Fatal("idle with no watches up")
	}

	p.setPresenceUp(true)
	p.setApprovalsUp(true)
	p.presence(idleEv("athena@acme", messagingv1.Presence_PRESENCE_IDLE))
	clk.Add(IdleAfter - time.Second)
	p.presence(idleEv("athena@acme", messagingv1.Presence_PRESENCE_IDLE)) // heartbeat
	if isIdle(t, p, "athena") {
		t.Fatal("idle before IdleAfter")
	}
	clk.Add(time.Second)
	if !isIdle(t, p, "athena") {
		t.Fatal("not idle after IdleAfter")
	}
	if isIdle(t, p, "hermes") {
		t.Fatal("a room never seen is idle")
	}

	// Working resets the clock.
	p.presence(idleEv("athena@acme", messagingv1.Presence_PRESENCE_WORKING))
	p.presence(idleEv("athena@acme", messagingv1.Presence_PRESENCE_IDLE))
	clk.Add(time.Minute)
	if isIdle(t, p, "athena") {
		t.Fatal("idle a minute after working")
	}
	clk.Add(time.Minute)
	if !isIdle(t, p, "athena") {
		t.Fatal("not idle again")
	}

	// A waiting approval keeps the room busy until answered or withdrawn.
	req := func(id string, at time.Time) *messagingv1.ApprovalEvent {
		return &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Requested{Requested: &messagingv1.ApprovalRequest{
			Id: id, Room: "athena@acme", RequestedAt: timestamppb.New(at)}}}
	}
	p.approval(req("a1", clk.Now()))
	if isIdle(t, p, "athena") {
		t.Fatal("idle with an approval waiting")
	}
	p.approval(&messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Answered{Answered: &messagingv1.ApprovalAnswer{ApprovalId: "a1"}}})
	if !isIdle(t, p, "athena") {
		t.Fatal("still busy after the answer")
	}
	p.approval(req("a2", clk.Now()))
	p.approval(&messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Withdrawn{Withdrawn: &messagingv1.WithdrawApprovalRequest{ApprovalId: "a2"}}})
	if !isIdle(t, p, "athena") {
		t.Fatal("still busy after the withdrawal")
	}
	p.approval(req("old", clk.Now().Add(-25*time.Hour)))
	if !isIdle(t, p, "athena") {
		t.Fatal("an approval past the timeout keeps the room busy")
	}

	// A lost watch: nothing is idle, and the clocks start over.
	p.setPresenceUp(false)
	if isIdle(t, p, "athena") {
		t.Fatal("idle without the presence watch")
	}
	p.setPresenceUp(true)
	p.presence(idleEv("athena@acme", messagingv1.Presence_PRESENCE_IDLE))
	if isIdle(t, p, "athena") {
		t.Fatal("idle right after reconnecting")
	}
	clk.Add(IdleAfter)
	if !isIdle(t, p, "athena") {
		t.Fatal("not idle after reconnecting and waiting")
	}
	p.setApprovalsUp(false)
	p.setApprovalsUp(true)
	if isIdle(t, p, "athena") {
		t.Fatal("idle right after the approvals watch reconnected")
	}
}

// fakeMessaging serves the two watches to the operator only.
type fakeMessaging struct {
	messagingv1connect.UnimplementedMessagingServiceHandler
	presence  chan *messagingv1.PresenceEvent
	approvals chan *messagingv1.ApprovalEvent
	drop      chan struct{} // ends the presence stream
}

func operatorOnly(ctx context.Context) error {
	if c, _ := msgserver.CallerFrom(ctx); !c.Operator {
		return connect.NewError(connect.CodePermissionDenied, nil)
	}
	return nil
}

func (f *fakeMessaging) WatchPresence(ctx context.Context, _ *connect.Request[messagingv1.WatchPresenceRequest], s *connect.ServerStream[messagingv1.PresenceEvent]) error {
	if err := operatorOnly(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-f.drop:
			return connect.NewError(connect.CodeUnavailable, nil)
		case ev := <-f.presence:
			if err := s.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (f *fakeMessaging) WatchApprovals(ctx context.Context, _ *connect.Request[messagingv1.WatchApprovalsRequest], s *connect.ServerStream[messagingv1.ApprovalEvent]) error {
	if err := operatorOnly(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-f.approvals:
			if err := s.Send(ev); err != nil {
				return err
			}
		}
	}
}

// TestPresenceIdleOverMessaging dials a fake messaging service over mTLS
// with the operator's certificate, checked by messaging's own identity
// middleware.
func TestPresenceIdleOverMessaging(t *testing.T) {
	now := time.Now()
	ca, err := pki.NewCA("test-ca", now)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Issue(pki.Request{CommonName: "messaging", IPs: []net.IP{net.ParseIP("127.0.0.1")}}, now)
	if err != nil {
		t.Fatal(err)
	}
	serving, err := tls.X509KeyPair(leaf.CertPEM, leaf.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CABundle())

	f := &fakeMessaging{presence: make(chan *messagingv1.PresenceEvent), approvals: make(chan *messagingv1.ApprovalEvent), drop: make(chan struct{})}
	mux := http.NewServeMux()
	mux.Handle(messagingv1connect.NewMessagingServiceHandler(f))
	srv := httptest.NewUnstartedServer(msgserver.Identify(mux, false))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serving}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	srv.StartTLS()
	defer srv.Close()

	p, err := NewPresenceIdle(ca, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: now}
	p.Now = clk.Now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = p.Start(ctx); close(done) }()

	f.presence <- idleEv("athena@acme", messagingv1.Presence_PRESENCE_IDLE)
	f.approvals <- &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Requested{Requested: &messagingv1.ApprovalRequest{
		Id: "z1", Room: "zeus@acme", RequestedAt: timestamppb.New(clk.Now())}}}
	eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		_, seen := p.idleSince["athena@acme"]
		_, waiting := p.pending["z1"]
		return seen && waiting
	}, "both watches delivering")
	clk.Add(IdleAfter)
	if !isIdle(t, p, "athena") {
		t.Fatal("room not idle over messaging")
	}

	f.approvals <- &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Requested{Requested: &messagingv1.ApprovalRequest{
		Id: "a1", Room: "athena@acme", RequestedAt: timestamppb.New(clk.Now())}}}
	eventually(t, func() bool { return !isIdle(t, p, "athena") }, "approval seen")
	f.approvals <- &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Answered{Answered: &messagingv1.ApprovalAnswer{ApprovalId: "a1"}}}
	eventually(t, func() bool { return isIdle(t, p, "athena") }, "answer seen")

	f.drop <- struct{}{}
	eventually(t, func() bool { return !isIdle(t, p, "athena") }, "lost watch noticed")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return")
	}
}

// TestIdleRolloutWithPresence: under whenIdle, a new image reaches a room
// only after it has been idle for IdleAfter, one room at a time, and a room
// still waiting is looked at again within a minute.
func TestIdleRolloutWithPresence(t *testing.T) {
	e := newEnv(t)
	p := &PresenceIdle{Now: func() time.Time { return e.now }}
	p.setPresenceUp(true)
	p.setApprovalsUp(true)
	e.r.Idle = p
	e.addRoom("athena", nil)
	e.addRoom("hermes", nil)
	e.reconcile("athena", "hermes")

	image := func(n string) string { return e.sts(n).Spec.Template.Spec.Containers[0].Image }
	e.now = e.now.Add(time.Hour) // the approvals watch has been up a while
	p.presence(idleEv("athena@acme", messagingv1.Presence_PRESENCE_IDLE))
	p.presence(idleEv("hermes@acme", messagingv1.Presence_PRESENCE_WORKING))
	e.images.RoomImage = "room:2"
	e.reconcile("athena", "hermes")
	if image("athena") != "room:1" {
		t.Fatal("rolled before the room was idle long enough")
	}

	e.now = e.now.Add(IdleAfter)
	e.reconcile("athena", "hermes")
	if image("athena") != "room:2" {
		t.Fatal("idle room not rolled")
	}
	if image("hermes") != "room:1" {
		t.Fatal("working room rolled")
	}

	p.presence(idleEv("hermes@acme", messagingv1.Presence_PRESENCE_IDLE))
	e.now = e.now.Add(IdleAfter / 2)
	res, err := e.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "hermes"}})
	if err != nil {
		t.Fatal(err)
	}
	if image("hermes") != "room:1" {
		t.Fatal("rolled before its idle time")
	}
	if res.RequeueAfter == 0 || res.RequeueAfter > time.Minute {
		t.Fatalf("pending roll requeued after %v", res.RequeueAfter)
	}
	e.now = e.now.Add(IdleAfter / 2)
	e.reconcile("hermes")
	if image("hermes") != "room:2" {
		t.Fatal("second idle room not rolled")
	}
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
