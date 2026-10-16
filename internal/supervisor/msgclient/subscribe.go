package msgclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
)

// Handler processes a batch of deliveries (items that arrived together or
// while the previous batch waited); nil means all may be acked.
type Handler func(ctx context.Context, batch []*messagingv1.Delivery) error

// Subscribe keeps a Subscribe stream open (reconnecting with backoff) and
// passes deliveries to h in batches. A failed batch is retried with backoff,
// together with anything that arrived meanwhile, and every item is acked
// only after h succeeded. Messages, posts and notices already in dedup are
// acked without h.
func Subscribe(ctx context.Context, c messagingv1connect.MessagingServiceClient, dedup *Dedup, h Handler, log *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := subscribeOnce(ctx, c, dedup, h, log)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Warn("messaging stream ended; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// DeliveryKey is the dedup key of a delivery ("" for items not deduped).
func DeliveryKey(d *messagingv1.Delivery) string {
	switch {
	case d.GetMessage() != nil:
		return "m:" + d.GetMessage().GetId()
	case d.GetNotice() != nil:
		return "n:" + d.GetNotice().GetId()
	case d.GetPost() != nil:
		return "p:" + d.GetPost().GetId()
	}
	return ""
}

func drain(items <-chan *messagingv1.Delivery, batch []*messagingv1.Delivery) []*messagingv1.Delivery {
	for {
		select {
		case d := <-items:
			batch = append(batch, d)
		default:
			return batch
		}
	}
}

func subscribeOnce(ctx context.Context, c messagingv1connect.MessagingServiceClient, dedup *Dedup, h Handler, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.Subscribe(ctx, connect.NewRequest(&messagingv1.SubscribeRequest{}))
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	log.Info("subscribed to messaging")
	items := make(chan *messagingv1.Delivery, 256)
	ended := make(chan error, 1)
	go func() {
		for stream.Receive() {
			select {
			case items <- stream.Msg():
			case <-ctx.Done():
				return
			}
		}
		if err := stream.Err(); err != nil {
			ended <- err
			return
		}
		ended <- errors.New("stream closed by server")
	}()
	for {
		var batch []*messagingv1.Delivery
		select {
		case d := <-items:
			batch = append(batch, d)
		case err := <-ended:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
		batch = drain(items, batch)
		backoff := time.Second
		for {
			var fresh []*messagingv1.Delivery
			for _, d := range batch {
				if k := DeliveryKey(d); k == "" || dedup == nil || !dedup.Seen(k) {
					fresh = append(fresh, d)
				}
			}
			if len(fresh) == 0 {
				break
			}
			err := h(ctx, fresh)
			if err == nil {
				break
			}
			log.Warn("delivering to the agent failed; retrying", "items", len(fresh), "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			batch = drain(items, batch)
		}
		for _, d := range batch {
			if k := DeliveryKey(d); k != "" && dedup != nil {
				if err := dedup.Add(k); err != nil {
					log.Warn("dedup set", "err", err)
				}
			}
			if _, err := c.Ack(ctx, connect.NewRequest(&messagingv1.AckRequest{DeliveryId: d.GetDeliveryId()})); err != nil {
				return fmt.Errorf("ack: %w", err)
			}
		}
	}
}
