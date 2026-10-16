package server

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/messaging/policy"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
)

// urgent reports whether an item is never held back by the push budget:
// approval answers, platform notices and people's direct messages.
func urgent(d *messagingv1.Delivery) bool {
	return d.Urgent || d.GetApprovalAnswer() != nil || d.GetNotice() != nil ||
		d.GetMessage().GetFromKind() == string(policy.Human)
}

// budget wraps send with the per-recipient push budget (docs/design.md
// §7.8). Items over budget are acked here, unpushed: they stay unread for
// read_messages, and the agent gets one notice per overflow window, which
// ends when an item is pushed again. People's inboxes have no budget.
func (s *Server) budget(me *party, send func(*messagingv1.Delivery) error) func(*store.Pending) error {
	if me.p.Kind != policy.Agent {
		return func(p *store.Pending) error { return send(p.Delivery) }
	}
	overflowing := false
	return func(p *store.Pending) error {
		d := p.Delivery
		if urgent(d) {
			return send(d)
		}
		// A redelivery was paid for when first pushed.
		per := me.ws.PushPerMinute()
		if ws, ok := s.dir.Workspace(me.ws.Name); ok {
			per = ws.PushPerMinute()
		}
		if p.Delivered <= 1 && !s.lim.AllowN("push/"+me.addr, 1, per, time.Now()) {
			ctx := context.Background()
			if err := p.Ack(ctx); err != nil {
				return err
			}
			if !overflowing {
				overflowing = true
				s.overflowNotice(ctx, me, int(p.Waiting)+1)
			}
			return nil
		}
		overflowing = false
		return send(d)
	}
}

// overflowNotice tells an agent that n items wait unread. It goes through
// the inbox, after the items waiting now.
func (s *Server) overflowNotice(ctx context.Context, me *party, n int) {
	id := ulid.Make().String()
	text := fmt.Sprintf("%d more messages are waiting; read them with read_messages", n)
	if n == 1 {
		text = "1 more message is waiting; read it with read_messages"
	}
	d := &messagingv1.Delivery{Urgent: true, Item: &messagingv1.Delivery_Notice{Notice: &messagingv1.Notice{Id: id, Text: text}}}
	if _, err := s.st.Deliver(ctx, me.ws.Name, me.p.Name, "overflow/"+id, d); err != nil {
		s.log.Warn("overflow notice", "to", me.addr, "err", err)
	}
	s.log.Info("push budget exceeded", "to", me.addr, "waiting", n)
}
