package store

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// PublishApproval appends an approval event to approval.<ws>.<room>.
func (s *Store) PublishApproval(ctx context.Context, ws, room, msgID string, ev *messagingv1.ApprovalEvent) (bool, error) {
	return s.publish(ctx, "approval."+ws+"."+room, msgID, ev)
}

// WatchApprovals replays the workspace's pending approval requests to emit,
// then streams every later event, until ctx ends or emit fails.
func (s *Store) WatchApprovals(ctx context.Context, ws string, emit func(*messagingv1.ApprovalEvent) error) error {
	stream, err := s.js.Stream(ctx, ApprovalStream(ws))
	if err != nil {
		return err
	}
	cons, err := stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{DeliverPolicy: jetstream.DeliverAllPolicy})
	if err != nil {
		return err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return err
	}
	it, err := cons.Messages()
	if err != nil {
		return err
	}
	defer it.Stop()

	// Replay: everything up to the last sequence at connect time.
	replayTo := info.State.LastSeq
	pending := map[string]*messagingv1.ApprovalEvent{}
	var order []string
	replaying := info.State.Msgs > 0
	for {
		if !replaying && pending != nil {
			for _, id := range order {
				if ev := pending[id]; ev != nil {
					if err := emit(ev); err != nil {
						return err
					}
				}
			}
			pending = nil
		}
		m, err := it.Next(jetstream.NextContext(ctx))
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, jetstream.ErrNoHeartbeat) {
				continue
			}
			return err
		}
		var ev messagingv1.ApprovalEvent
		if err := proto.Unmarshal(m.Data(), &ev); err != nil {
			continue
		}
		if !replaying {
			if err := emit(&ev); err != nil {
				return err
			}
			continue
		}
		switch e := ev.Event.(type) {
		case *messagingv1.ApprovalEvent_Requested:
			id := e.Requested.GetId()
			if _, seen := pending[id]; !seen {
				order = append(order, id)
			}
			pending[id] = &ev
		case *messagingv1.ApprovalEvent_Answered:
			pending[e.Answered.GetApprovalId()] = nil
		case *messagingv1.ApprovalEvent_Withdrawn:
			pending[e.Withdrawn.GetApprovalId()] = nil
		}
		if meta, err := m.Metadata(); err == nil && meta.Sequence.Stream >= replayTo {
			replaying = false
		}
	}
}
