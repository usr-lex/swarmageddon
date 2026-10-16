package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/msgclient"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// InboxHub caches people's messages from messaging and pushes them to open pages.
type InboxHub struct {
	mu   sync.Mutex
	subs map[chan store.InboxMessage]string // channel -> user ID
}

func (h *InboxHub) publish(m store.InboxMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, uid := range h.subs {
		if uid == m.UserID {
			select {
			case ch <- m:
			default:
			}
		}
	}
}

func (h *InboxHub) subscribe(uid string) (<-chan store.InboxMessage, func()) {
	ch := make(chan store.InboxMessage, 64)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan store.InboxMessage]string{}
	}
	h.subs[ch] = uid
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// followInbox reads one person's inbox in one workspace from messaging
// (as handle@workspace), stores each message, pushes it, and acks it.
func (s *Server) followInbox(ctx context.Context, u store.User, ws string) error {
	addr := u.Handle + "@" + ws
	req := connect.NewRequest(&messagingv1.SubscribeRequest{})
	req.Header().Set(msgclient.OnBehalfHeader, addr)
	stream, err := s.Msg.Subscribe(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		d := stream.Msg()
		db := s.DB()
		if db == nil {
			return errors.New("database gone")
		}
		if m := d.GetMessage(); m != nil {
			at := time.Now().UTC()
			if m.GetSentAt() != nil {
				at = m.GetSentAt().AsTime()
			}
			im := store.InboxMessage{UserID: u.ID, ID: m.GetId(), Workspace: ws, From: m.GetFrom(), To: m.GetTo(), Body: m.GetBody(),
				ReplyTo: m.GetReplyTo(), SentAt: at, Direction: "in"}
			isNew, err := db.InsertInbox(ctx, im)
			if err != nil {
				return err
			}
			if isNew {
				s.Inbox.publish(im)
				if subs, err := db.PushSubscriptions(ctx, u.ID); err == nil && len(subs) > 0 {
					body := m.GetBody()
					if len(body) > 200 {
						body = body[:200] + "…"
					}
					s.Notify.PushTo(ctx, subs, notify.Event{Kind: "message", Workspace: ws, Title: "Message from " + m.GetFrom(), Body: body, URL: "/inbox"})
				}
			}
		} else if n := d.GetNotice(); n != nil {
			im := store.InboxMessage{UserID: u.ID, ID: "notice-" + n.GetId(), Workspace: ws, From: "platform@" + ws, To: addr,
				Body: n.GetText(), SentAt: time.Now().UTC(), Direction: "in"}
			if isNew, err := db.InsertInbox(ctx, im); err == nil && isNew {
				s.Inbox.publish(im)
			}
		}
		ack := connect.NewRequest(&messagingv1.AckRequest{DeliveryId: d.GetDeliveryId()})
		ack.Header().Set(msgclient.OnBehalfHeader, addr)
		if _, err := s.Msg.Ack(ctx, ack); err != nil {
			return err
		}
	}
	return stream.Err()
}

// Subscriptions keeps one goroutine per key running, starting and stopping
// them as the wanted set changes.
type Subscriptions struct {
	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// Reconcile starts wanted keys that aren't running and stops the rest.
func (m *Subscriptions) Reconcile(ctx context.Context, want map[string]func(ctx context.Context) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running == nil {
		m.running = map[string]context.CancelFunc{}
	}
	for k, cancel := range m.running {
		if _, ok := want[k]; !ok {
			cancel()
			delete(m.running, k)
		}
	}
	for k, fn := range want {
		if _, ok := m.running[k]; ok {
			continue
		}
		cctx, cancel := context.WithCancel(ctx)
		m.running[k] = cancel
		go msgclient.Loop(cctx, k, fn)
	}
}

// SubscriptionLoop keeps people's inboxes and the status@ inboxes followed.
func (s *Server) SubscriptionLoop(ctx context.Context, reports func(ctx context.Context, ws string) error) {
	var subs Subscriptions
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		db := s.DB()
		if db != nil && s.Msg != nil {
			want := map[string]func(context.Context) error{}
			wss, err := s.K.Workspaces(ctx)
			if err == nil {
				users, _ := db.ListUsers(ctx)
				for _, w := range wss {
					ws := w.Name
					want["status@"+ws] = func(ctx context.Context) error { return reports(ctx, ws) }
					for _, u := range users {
						if u.Disabled || u.Provider == "setup" {
							continue
						}
						assigned, _ := db.Assignments(ctx, u.ID)
						p := &auth.Principal{Handle: u.Handle, Email: u.Email, Role: auth.ParseRole(u.Role()), Workspaces: assigned}
						if !p.CanView(ws, w.Spec.Owners) {
							continue
						}
						u := u
						want[u.Handle+"@"+ws] = func(ctx context.Context) error { return s.followInbox(ctx, u, ws) }
					}
				}
			}
			subs.Reconcile(ctx, want)
		}
		select {
		case <-ctx.Done():
			subs.Reconcile(ctx, nil)
			return
		case <-t.C:
		}
	}
}

type inboxSvc struct{ *Server }

func inboxProto(m store.InboxMessage) *ccv1.InboxMessage {
	return &ccv1.InboxMessage{Id: m.ID, Workspace: m.Workspace, From: m.From, To: m.To, Body: m.Body, ReplyTo: m.ReplyTo,
		SentAt: ts(m.SentAt), Read: m.Read, Direction: m.Direction}
}

func (s inboxSvc) user(ctx context.Context) (*auth.Principal, *store.Store, error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, nil, err
	}
	db := s.DB()
	if db == nil || p.UserID == "" {
		return nil, nil, errNoDB()
	}
	return p, db, nil
}

func (s inboxSvc) ListInbox(ctx context.Context, req *connect.Request[ccv1.ListInboxRequest]) (*connect.Response[ccv1.ListInboxResponse], error) {
	p, db, err := s.user(ctx)
	if err != nil {
		return nil, err
	}
	var before time.Time
	if req.Msg.GetBefore() != nil {
		before = req.Msg.GetBefore().AsTime()
	}
	msgs, unread, err := db.ListInbox(ctx, p.UserID, req.Msg.GetWorkspace(), req.Msg.GetPeer(), before, int(req.Msg.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := &ccv1.ListInboxResponse{Unread: int32(unread)}
	for _, m := range msgs {
		out.Messages = append(out.Messages, inboxProto(m))
	}
	return connect.NewResponse(out), nil
}

func (s inboxSvc) SendMessage(ctx context.Context, req *connect.Request[ccv1.SendMessageRequest]) (*connect.Response[ccv1.SendMessageResponse], error) {
	ws := req.Msg.GetWorkspace()
	p, err := s.canOperate(ctx, ws)
	if err != nil {
		return nil, err
	}
	if s.Msg == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("messaging is not connected"))
	}
	body := strings.TrimSpace(req.Msg.GetBody())
	if body == "" {
		return nil, errInvalid("the message is empty")
	}
	to := req.Msg.GetTo()
	if !strings.Contains(to, "@") {
		to += "@" + ws
	}
	from := p.Handle + "@" + ws
	id := ulid.Make().String()
	_, err = s.Msg.SendAs(ctx, from, id, to, body, req.Msg.GetReplyTo())
	s.audit(ctx, req, "message.send", "address/"+to, ws, nil, map[string]any{"id": id, "bytes": len(body)}, err)
	if err != nil {
		return nil, err // messaging's refusal explains which rule refused it
	}
	m := store.InboxMessage{UserID: p.UserID, ID: id, Workspace: ws, From: from, To: to, Body: body, ReplyTo: req.Msg.GetReplyTo(),
		SentAt: time.Now().UTC(), Direction: "out", Read: true}
	if db := s.DB(); db != nil && p.UserID != "" {
		_, _ = db.InsertInbox(ctx, m)
		s.Inbox.publish(m)
	}
	return connect.NewResponse(&ccv1.SendMessageResponse{Message: inboxProto(m)}), nil
}

func (s inboxSvc) MarkRead(ctx context.Context, req *connect.Request[ccv1.MarkReadRequest]) (*connect.Response[ccv1.MarkReadResponse], error) {
	p, db, err := s.user(ctx)
	if err != nil {
		return nil, err
	}
	if err := db.MarkRead(ctx, p.UserID, req.Msg.GetIds()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.MarkReadResponse{}), nil
}

func (s inboxSvc) WatchInbox(ctx context.Context, req *connect.Request[ccv1.WatchInboxRequest], stream *connect.ServerStream[ccv1.WatchInboxResponse]) error {
	p, _, err := s.user(ctx)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, w := range req.Msg.GetWorkspaces() {
		want[w] = true
	}
	ch, cancel := s.Inbox.subscribe(p.UserID)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m, ok := <-ch:
			if !ok {
				return nil
			}
			if len(want) > 0 && !want[m.Workspace] {
				continue
			}
			if err := stream.Send(&ccv1.WatchInboxResponse{Message: inboxProto(m)}); err != nil {
				return err
			}
		}
	}
}
