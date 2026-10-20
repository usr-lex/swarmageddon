package server

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/messaging/policy"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
)

// ReportVerdict implements MessagingService: it records a warden verdict in
// the audit log (ADR 0011). A room reports only about itself, and its
// verdicts are marked as self-reports; the gateway guard reports about any
// room. The full verdict is the record's body, as protobuf JSON.
func (s *Server) ReportVerdict(ctx context.Context, req *connect.Request[messagingv1.ReportVerdictRequest]) (*connect.Response[messagingv1.ReportVerdictResponse], error) {
	v := req.Msg.Verdict
	if v == nil || v.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("verdict with an id is required"))
	}
	c, _ := CallerFrom(ctx)
	var ws string
	switch {
	case c.Kind == policy.Agent:
		v.Room, v.Source, ws = c.Address(), "room", c.Workspace
	case c.Warden:
		name, w, err := policy.SplitAddress(v.Room)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("room: %w", err))
		}
		dir, ok := s.dir.Workspace(w)
		if _, room := dir.Rooms[name]; !ok || !room {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no room %s", v.Room))
		}
		v.Source, ws = "gateway", w
	default:
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only rooms and the warden report verdicts"))
	}
	// The reporter's time, never later than ours: the audit is read from
	// a cursor on these times, and a record dated ahead of when it was
	// stored stops the control center's follower until the clock gets there.
	if now := timestamppb.Now(); v.At == nil || v.At.AsTime().After(now.AsTime()) {
		v.At = now
	}
	body, err := protojson.Marshal(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	rec := &messagingv1.AuditRecord{
		Kind: store.AuditVerdict, At: v.At, From: v.Room, MessageId: v.Id, Body: string(body), Link: v.Action,
		Reason: fmt.Sprintf("%s %s/%s: %s", v.Severity, v.Layer, v.Rule, v.Reason),
	}
	if err := s.st.Audit(ctx, ws, rec); err != nil {
		return nil, unavailable(err)
	}
	return connect.NewResponse(&messagingv1.ReportVerdictResponse{}), nil
}
