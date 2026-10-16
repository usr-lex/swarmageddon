package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// Deliver stores d in a recipient's inbox. msgID is the dedup key; dup is
// true if it was already stored within the dedup window.
func (s *Store) Deliver(ctx context.Context, ws, recipient, msgID string, d *messagingv1.Delivery) (dup bool, err error) {
	return s.publish(ctx, InboxSubject(ws, recipient), msgID, d)
}

// ErrBadDelivery is returned by Ack for a delivery id the caller doesn't own.
var ErrBadDelivery = errors.New("not a delivery of this inbox")

// Subscribe streams a recipient's inbox to fn, one unacked delivery at a
// time, through the recipient's durable consumer, until ctx ends or fn fails.
// The delivery id is the JetStream ack subject, so Ack works on any replica.
func (s *Store) Subscribe(ctx context.Context, ws, name string, fn func(deliveryID string, d *messagingv1.Delivery) error) error {
	stream, err := s.js.Stream(ctx, InboxStream(ws))
	if err != nil {
		return err
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: InboxSubject(ws, name),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       s.AckWait,
		MaxAckPending: 1,
		MaxDeliver:    -1,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return fmt.Errorf("consumer %s: %w", name, err)
	}
	it, err := cons.Messages(jetstream.PullMaxMessages(1), jetstream.PullHeartbeat(2*time.Second), jetstream.PullExpiry(10*time.Second))
	if err != nil {
		return err
	}
	defer it.Stop()
	var last jetstream.Msg
	defer func() {
		// The client may not have received the last delivery; hand it back
		// for redelivery now rather than after AckWait. A nak of an acked
		// message is ignored by the server.
		if last != nil {
			_ = last.Nak()
		}
	}()
	for {
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
		var d messagingv1.Delivery
		if err := proto.Unmarshal(m.Data(), &d); err != nil {
			s.log.Error("dropping undecodable inbox message", "subject", m.Subject(), "err", err)
			_ = m.Term()
			continue
		}
		d.DeliveryId = m.Reply()
		last = m
		if err := fn(m.Reply(), &d); err != nil {
			return err
		}
	}
}

// Ack acknowledges a delivery of ws/name's inbox.
func (s *Store) Ack(ctx context.Context, ws, name, deliveryID string) error {
	stream, consumer, ok := parseAckSubject(deliveryID)
	if !ok || stream != InboxStream(ws) || consumer != name {
		return ErrBadDelivery
	}
	_, err := s.nc.RequestWithContext(ctx, deliveryID, []byte("+ACK"))
	return err
}

// parseAckSubject reads the stream and consumer from a JetStream ack subject:
// $JS.ACK.<stream>.<consumer>.<n>.<sseq>.<cseq>.<ts>.<pending> or the form
// with domain and account hash after ACK.
func parseAckSubject(s string) (stream, consumer string, ok bool) {
	t := strings.Split(s, ".")
	if len(t) < 9 || t[0] != "$JS" || t[1] != "ACK" {
		return "", "", false
	}
	if len(t) == 9 {
		return t[2], t[3], true
	}
	if len(t) >= 11 {
		return t[4], t[5], true
	}
	return "", "", false
}

// ExpireUndelivered sends a "not delivered" notice for each message in a
// workspace's inbox that would expire within margin without having been
// acked, then deletes it. Only the stream leader acts; it is safe to call on
// every replica. Best effort.
func (s *Store) ExpireUndelivered(ctx context.Context, ws string, maxAge, margin time.Duration) error {
	if maxAge <= 0 || !s.isLeader(ctx, InboxStream(ws)) {
		return nil
	}
	stream, err := s.js.Stream(ctx, InboxStream(ws))
	if err != nil {
		return err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-(maxAge - margin))
	floors := map[string]uint64{} // consumer name -> highest delivered stream seq
	for seq := info.State.FirstSeq; seq > 0 && seq <= info.State.LastSeq; seq++ {
		raw, err := stream.GetMsg(ctx, seq)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if raw.Time.After(cutoff) {
			break
		}
		name := raw.Subject[strings.LastIndexByte(raw.Subject, '.')+1:]
		floor, ok := floors[name]
		if !ok {
			if c, err := stream.Consumer(ctx, name); err == nil {
				if ci, err := c.Info(ctx); err == nil {
					// Delivered covers acked and in flight; with MaxAckPending
					// 1 everything below it has been handed out in order.
					floor = ci.Delivered.Stream
				}
			}
			floors[name] = floor
		}
		if seq <= floor {
			continue
		}
		var d messagingv1.Delivery
		if err := proto.Unmarshal(raw.Data, &d); err == nil {
			s.notifyExpired(ctx, ws, name, d.GetMessage(), maxAge)
		}
		if err := stream.DeleteMsg(ctx, seq); err != nil && !errors.Is(err, jetstream.ErrMsgNotFound) {
			return err
		}
	}
	return nil
}

func (s *Store) notifyExpired(ctx context.Context, ws, recipient string, m *messagingv1.Message, maxAge time.Duration) {
	if m == nil {
		return // approval answers and notices expire silently
	}
	sender, sws, ok := strings.Cut(m.GetFrom(), "@")
	if !ok || sws != ws {
		return
	}
	notice := &messagingv1.Delivery{Item: &messagingv1.Delivery_Notice{Notice: &messagingv1.Notice{
		Id: ulid.Make().String(),
		Text: fmt.Sprintf("Not delivered: %s@%s did not pick up your message within %s, so it expired. It began: %q",
			recipient, ws, maxAge, preview(m.GetBody())),
		AboutMessageId: m.GetId(),
	}}}
	if _, err := s.Deliver(ctx, ws, sender, "notdelivered/"+m.GetId()+"/"+recipient, notice); err != nil {
		s.log.Warn("not-delivered notice", "workspace", ws, "to", sender, "err", err)
	}
}

func preview(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

// now is a hook for timestamps.
func now() *timestamppb.Timestamp { return timestamppb.Now() }
