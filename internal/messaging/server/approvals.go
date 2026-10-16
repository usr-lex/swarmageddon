package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/messaging/policy"
)

// RequestApproval implements MessagingService.
func (s *Server) RequestApproval(ctx context.Context, req *connect.Request[messagingv1.RequestApprovalRequest]) (*connect.Response[messagingv1.RequestApprovalResponse], error) {
	me, err := s.room(ctx)
	if err != nil {
		return nil, err
	}
	r := req.Msg.Request
	if r == nil || r.Id == "" || len(r.Id) > 128 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request with an id is required"))
	}
	r.Room = me.addr // rooms can't ask for others
	if r.RequestedAt == nil {
		r.RequestedAt = timestamppb.Now()
	}
	ev := &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Requested{Requested: r}}
	if _, err := s.st.PublishApproval(ctx, me.ws.Name, me.p.Name, "req/"+r.Id, ev); err != nil {
		return nil, unavailable(err)
	}
	return connect.NewResponse(&messagingv1.RequestApprovalResponse{}), nil
}

// WithdrawApproval implements MessagingService.
func (s *Server) WithdrawApproval(ctx context.Context, req *connect.Request[messagingv1.WithdrawApprovalRequest]) (*connect.Response[messagingv1.WithdrawApprovalResponse], error) {
	me, err := s.room(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.ApprovalId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("approval_id is required"))
	}
	ev := &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Withdrawn{Withdrawn: req.Msg}}
	if _, err := s.st.PublishApproval(ctx, me.ws.Name, me.p.Name, "wd/"+req.Msg.ApprovalId, ev); err != nil {
		return nil, unavailable(err)
	}
	return connect.NewResponse(&messagingv1.WithdrawApprovalResponse{}), nil
}

// AnswerApproval implements MessagingService. The answer reaches the room as
// a Delivery in its inbox; the first answer wins.
func (s *Server) AnswerApproval(ctx context.Context, req *connect.Request[messagingv1.AnswerApprovalRequest]) (*connect.Response[messagingv1.AnswerApprovalResponse], error) {
	c, err := controlCenter(ctx)
	if err != nil {
		return nil, err
	}
	a := req.Msg.Answer
	if a == nil || a.ApprovalId == "" || a.Decision == messagingv1.Decision_DECISION_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("answer with approval_id and decision is required"))
	}
	room, wsName, err := policy.SplitAddress(req.Msg.Room)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("room: %w", err))
	}
	ws, ok := s.dir.Workspace(wsName)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("there is no workspace %s", wsName))
	}
	if _, ok := ws.Rooms[room]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("there is no room %s", req.Msg.Room))
	}
	if err := s.ensure(ctx, ws); err != nil {
		return nil, unavailable(err)
	}
	if c.Name != "" {
		a.AnsweredBy = c.Address()
	}
	if a.AnsweredAt == nil {
		a.AnsweredAt = timestamppb.Now()
	}
	d := &messagingv1.Delivery{Urgent: true, Item: &messagingv1.Delivery_ApprovalAnswer{ApprovalAnswer: a}}
	dup, err := s.st.Deliver(ctx, ws.Name, room, "answer/"+a.ApprovalId, d)
	if err != nil {
		return nil, unavailable(err)
	}
	if dup {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("approval %s was already answered", a.ApprovalId))
	}
	ev := &messagingv1.ApprovalEvent{Event: &messagingv1.ApprovalEvent_Answered{Answered: a}}
	if _, err := s.st.PublishApproval(ctx, ws.Name, room, "ans/"+a.ApprovalId, ev); err != nil {
		return nil, unavailable(err)
	}
	return connect.NewResponse(&messagingv1.AnswerApprovalResponse{}), nil
}

// WatchApprovals implements MessagingService: pending requests first, then
// every request, answer and withdrawal. With no workspaces named it follows
// all of them, including ones created later.
func (s *Server) WatchApprovals(ctx context.Context, req *connect.Request[messagingv1.WatchApprovalsRequest], stream *connect.ServerStream[messagingv1.ApprovalEvent]) error {
	if _, err := controlCenter(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	// The watchers run in goroutines; none may touch the stream once this
	// handler returns, so stop them and wait for them first.
	var (
		mu     sync.Mutex
		closed bool
		wg     sync.WaitGroup
	)
	defer func() {
		cancel(nil)
		mu.Lock()
		closed = true
		mu.Unlock()
		wg.Wait()
	}()
	emit := func(ev *messagingv1.ApprovalEvent) error {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return context.Canceled
		}
		return stream.Send(ev)
	}
	if err := stream.Send(nil); err != nil {
		return err
	}
	watching := map[string]bool{}
	watch := func(name string) {
		ws, ok := s.dir.Workspace(name)
		if !ok || watching[name] {
			return
		}
		if err := s.ensure(ctx, ws); err != nil {
			s.log.Warn("ensure streams", "workspace", name, "err", err)
			return
		}
		watching[name] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.st.WatchApprovals(ctx, name, emit); err != nil && ctx.Err() == nil {
				cancel(err)
			}
		}()
	}
	for {
		names := req.Msg.Workspaces
		if len(names) == 0 {
			names = s.dir.Workspaces()
		}
		for _, n := range names {
			watch(n)
		}
		select {
		case <-ctx.Done():
			if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return unavailable(err)
			}
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}

// WatchPresence implements MessagingService.
func (s *Server) WatchPresence(ctx context.Context, req *connect.Request[messagingv1.WatchPresenceRequest], stream *connect.ServerStream[messagingv1.PresenceEvent]) error {
	if _, err := controlCenter(ctx); err != nil {
		return err
	}
	names := req.Msg.Workspaces
	if len(names) == 0 {
		names = s.dir.Workspaces()
	}
	var known []string
	for _, n := range names {
		if ws, ok := s.dir.Workspace(n); ok {
			for _, r := range ws.RoomNames() {
				known = append(known, policy.Address(r, n))
			}
		}
	}
	if err := stream.Send(nil); err != nil {
		return err
	}
	if err := s.st.WatchPresence(ctx, req.Msg.Workspaces, known, stream.Send); err != nil && ctx.Err() == nil {
		return unavailable(err)
	}
	return nil
}
