// Package msgfake is an in-memory messaging service for supervisor tests and
// canaries. It is not a policy engine.
package msgfake

import (
	"context"
	"fmt"
	"sync"

	"connectrpc.com/connect"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
)

// Server implements the messaging service handler for one subscriber.
type Server struct {
	messagingv1connect.UnimplementedMessagingServiceHandler

	// Peers is returned by ListPeers.
	Peers []*messagingv1.Peer
	// Self is ListPeers' self address.
	Self string
	// Answer, if set, answers each approval request automatically.
	Answer func(*messagingv1.ApprovalRequest) *messagingv1.ApprovalAnswer

	mu        sync.Mutex
	queue     chan *messagingv1.Delivery
	n         int
	sent      []*messagingv1.SendRequest
	acks      []string
	presence  []messagingv1.Presence
	status    []*messagingv1.RoomStatusReport
	approvals []*messagingv1.ApprovalRequest
	withdrawn []*messagingv1.WithdrawApprovalRequest
	verdicts  []*messagingv1.Verdict
	bcast     chan struct{}
}

// New returns an empty server.
func New() *Server {
	return &Server{queue: make(chan *messagingv1.Delivery, 100), bcast: make(chan struct{})}
}

func (s *Server) notify() {
	s.mu.Lock()
	close(s.bcast)
	s.bcast = make(chan struct{})
	s.mu.Unlock()
}

// Changed returns a channel closed at the next recorded call.
func (s *Server) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bcast
}

// WaitFor blocks until cond holds or ctx ends.
func (s *Server) WaitFor(ctx context.Context, cond func() bool) bool {
	for {
		ch := s.Changed()
		if cond() {
			return true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// Push queues a delivery for the subscriber; it returns the delivery ID.
func (s *Server) Push(d *messagingv1.Delivery) string {
	s.mu.Lock()
	s.n++
	d.DeliveryId = fmt.Sprintf("d%d", s.n)
	s.mu.Unlock()
	s.queue <- d
	return d.DeliveryId
}

// PushMessage queues a message.
func (s *Server) PushMessage(id, from, body string) string {
	return s.Push(&messagingv1.Delivery{Item: &messagingv1.Delivery_Message{Message: &messagingv1.Message{Id: id, From: from, Body: body, FromKind: "agent"}}})
}

// Snapshot returns copies of what was recorded.
func (s *Server) Snapshot() (sent []*messagingv1.SendRequest, acks []string, presence []messagingv1.Presence, status []*messagingv1.RoomStatusReport, approvals []*messagingv1.ApprovalRequest, withdrawn []*messagingv1.WithdrawApprovalRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(sent, s.sent...), append(acks, s.acks...), append(presence, s.presence...),
		append(status, s.status...), append(approvals, s.approvals...), append(withdrawn, s.withdrawn...)
}

// Subscribe streams queued deliveries. One delivery is outstanding at a time
// (like MaxAckPending 1): the next is sent after the previous is acked.
func (s *Server) Subscribe(ctx context.Context, _ *connect.Request[messagingv1.SubscribeRequest], st *connect.ServerStream[messagingv1.Delivery]) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case d := <-s.queue:
			if err := st.Send(d); err != nil {
				s.queue <- d
				return err
			}
			if !s.WaitFor(ctx, func() bool { return s.acked(d.DeliveryId) }) {
				s.queue <- d // redeliver to the next subscriber
				return nil
			}
		}
	}
}

func (s *Server) acked(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.acks {
		if a == id {
			return true
		}
	}
	return false
}

func (s *Server) Ack(_ context.Context, r *connect.Request[messagingv1.AckRequest]) (*connect.Response[messagingv1.AckResponse], error) {
	s.mu.Lock()
	s.acks = append(s.acks, r.Msg.GetDeliveryId())
	s.mu.Unlock()
	s.notify()
	return connect.NewResponse(&messagingv1.AckResponse{}), nil
}

func (s *Server) Send(_ context.Context, r *connect.Request[messagingv1.SendRequest]) (*connect.Response[messagingv1.SendResponse], error) {
	s.mu.Lock()
	s.sent = append(s.sent, r.Msg)
	s.mu.Unlock()
	s.notify()
	return connect.NewResponse(&messagingv1.SendResponse{Id: r.Msg.GetId()}), nil
}

func (s *Server) ReportPresence(_ context.Context, r *connect.Request[messagingv1.ReportPresenceRequest]) (*connect.Response[messagingv1.ReportPresenceResponse], error) {
	s.mu.Lock()
	s.presence = append(s.presence, r.Msg.GetPresence())
	s.mu.Unlock()
	s.notify()
	return connect.NewResponse(&messagingv1.ReportPresenceResponse{}), nil
}

func (s *Server) ReportStatus(_ context.Context, r *connect.Request[messagingv1.ReportStatusRequest]) (*connect.Response[messagingv1.ReportStatusResponse], error) {
	s.mu.Lock()
	s.status = append(s.status, r.Msg.GetStatus())
	s.mu.Unlock()
	s.notify()
	return connect.NewResponse(&messagingv1.ReportStatusResponse{}), nil
}

func (s *Server) ListPeers(context.Context, *connect.Request[messagingv1.ListPeersRequest]) (*connect.Response[messagingv1.ListPeersResponse], error) {
	return connect.NewResponse(&messagingv1.ListPeersResponse{Self: s.Self, Peers: s.Peers}), nil
}

func (s *Server) RequestApproval(_ context.Context, r *connect.Request[messagingv1.RequestApprovalRequest]) (*connect.Response[messagingv1.RequestApprovalResponse], error) {
	s.mu.Lock()
	s.approvals = append(s.approvals, r.Msg.GetRequest())
	s.mu.Unlock()
	s.notify()
	if s.Answer != nil {
		if a := s.Answer(r.Msg.GetRequest()); a != nil {
			go s.Push(&messagingv1.Delivery{Item: &messagingv1.Delivery_ApprovalAnswer{ApprovalAnswer: a}})
		}
	}
	return connect.NewResponse(&messagingv1.RequestApprovalResponse{}), nil
}

func (s *Server) WithdrawApproval(_ context.Context, r *connect.Request[messagingv1.WithdrawApprovalRequest]) (*connect.Response[messagingv1.WithdrawApprovalResponse], error) {
	s.mu.Lock()
	s.withdrawn = append(s.withdrawn, r.Msg)
	s.mu.Unlock()
	s.notify()
	return connect.NewResponse(&messagingv1.WithdrawApprovalResponse{}), nil
}

// ReportVerdict records a verdict.
func (s *Server) ReportVerdict(_ context.Context, r *connect.Request[messagingv1.ReportVerdictRequest]) (*connect.Response[messagingv1.ReportVerdictResponse], error) {
	s.mu.Lock()
	s.verdicts = append(s.verdicts, r.Msg.GetVerdict())
	s.mu.Unlock()
	s.notify()
	return connect.NewResponse(&messagingv1.ReportVerdictResponse{}), nil
}

// Verdicts returns the reported verdicts.
func (s *Server) Verdicts() []*messagingv1.Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*messagingv1.Verdict(nil), s.verdicts...)
}
