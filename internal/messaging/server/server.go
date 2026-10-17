// Package server implements swarm.messaging.v1.MessagingService over the
// store, the directory and the policy (docs/design.md §7).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
	"github.com/usr-lex/swarmageddon/internal/messaging/policy"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
)

// Server is the Connect handler.
type Server struct {
	// Answers Unimplemented for methods this server doesn't implement yet.
	messagingv1connect.UnimplementedMessagingServiceHandler

	dir directory.Directory
	st  *store.Store
	lim *policy.Limiter
	log *slog.Logger

	// ExpiryInterval and ExpiryMargin drive the "not delivered" scan: every
	// interval, messages within margin of expiry that nobody took are
	// returned to their sender as a notice.
	ExpiryInterval time.Duration
	ExpiryMargin   time.Duration
	// DigestTick is how often the inbox stream's leader looks for due
	// channel digests.
	DigestTick time.Duration
}

var _ messagingv1connect.MessagingServiceHandler = (*Server)(nil)

// New returns a Server.
func New(dir directory.Directory, st *store.Store, log *slog.Logger) *Server {
	return &Server{dir: dir, st: st, lim: policy.NewLimiter(), log: log,
		ExpiryInterval: 30 * time.Second, ExpiryMargin: time.Minute, DigestTick: 10 * time.Second}
}

// removeDeleted deletes the streams of workspaces that no longer exist. A
// workspace must be missing from a synced directory on two scans in a row,
// so a slow or restarting watch never costs a workspace its messages.
func (s *Server) removeDeleted(ctx context.Context, missing map[string]bool) {
	if !s.dir.Ready() {
		return
	}
	have, err := s.st.WorkspaceStreams(ctx)
	if err != nil {
		s.log.Warn("list streams", "err", err)
		return
	}
	now := map[string]bool{}
	for _, ws := range have {
		if _, ok := s.dir.Workspace(ws); ok {
			continue
		}
		if missing[ws] {
			if err := s.st.RemoveWorkspace(ctx, ws); err != nil {
				s.log.Warn("remove deleted workspace", "workspace", ws, "err", err)
				now[ws] = true
				continue
			}
			s.log.Info("removed the streams of a deleted workspace", "workspace", ws)
			continue
		}
		now[ws] = true
	}
	clear(missing)
	for ws := range now {
		missing[ws] = true
	}
}

// Run keeps every workspace's streams in line with its resource and runs the
// expiry scan and channel digests, until ctx ends.
func (s *Server) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.DigestTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runDigests(ctx)
			}
		}
	}()
	t := time.NewTicker(s.ExpiryInterval)
	defer t.Stop()
	missing := map[string]bool{}
	for {
		s.removeDeleted(ctx, missing)
		for _, name := range s.dir.Workspaces() {
			ws, ok := s.dir.Workspace(name)
			if !ok {
				continue
			}
			if err := s.ensure(ctx, ws); err != nil {
				s.log.Warn("ensure streams", "workspace", name, "err", err)
				continue
			}
			if err := s.st.ExpireUndelivered(ctx, name, ws.MessageRetention(), s.ExpiryMargin); err != nil {
				s.log.Warn("expiry scan", "workspace", name, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) ensure(ctx context.Context, ws *directory.Workspace) error {
	return s.st.EnsureWorkspace(ctx, ws.Name, store.WorkspaceConfig{
		MessageRetention:  ws.MessageRetention(),
		AuditRetention:    ws.AuditRetention(),
		ApprovalRetention: ws.ApprovalTimeout,
	})
}

// party is a caller resolved against the directory.
type party struct {
	ws   *directory.Workspace
	p    policy.Party
	addr string
}

// sender resolves a caller that sends or owns an inbox: a room, or the
// control center acting for a person, or for the system address named by
// system: forge@<ws> only sends, status@<ws> only has an inbox.
func (s *Server) sender(ctx context.Context, system string) (*party, error) {
	c, _ := CallerFrom(ctx)
	if c.ControlCenter && c.Name == "" {
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("the control center must name the person it acts for in %s", HeaderOnBehalfOf))
	}
	if c.Kind == policy.System && c.Name != system {
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("%s can't do this: forge@<workspace> only sends, status@<workspace> only receives", c.Address()))
	}
	ws, ok := s.dir.Workspace(c.Workspace)
	if !ok {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("there is no workspace %s", c.Workspace))
	}
	if err := s.ensure(ctx, ws); err != nil {
		return nil, unavailable(err)
	}
	pt := &party{ws: ws, addr: c.Address()}
	switch c.Kind {
	case policy.Agent:
		r, ok := ws.Rooms[c.Name]
		if !ok {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("there is no room %s in workspace %s", c.Name, ws.Name))
		}
		pt.p = policy.Party{Name: c.Name, Kind: policy.Agent, Group: r.Group}
	case policy.Human:
		if _, isRoom := ws.Rooms[c.Name]; isRoom {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s is a room, not a person", pt.addr))
		}
		if err := s.st.RegisterPerson(ctx, ws.Name, c.Name); err != nil {
			return nil, unavailable(err)
		}
		pt.p = policy.Party{Name: c.Name, Kind: policy.Human}
	case policy.System:
		pt.p = policy.Party{Name: c.Name, Kind: policy.System}
	}
	return pt, nil
}

// room resolves a caller that must be a room.
func (s *Server) room(ctx context.Context) (*party, error) {
	c, _ := CallerFrom(ctx)
	if c.Kind != policy.Agent {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only rooms may call this"))
	}
	return s.sender(ctx, "")
}

// controlCenter checks that the caller is the control center.
func controlCenter(ctx context.Context) (Caller, error) {
	c, _ := CallerFrom(ctx)
	if !c.ControlCenter {
		return c, connect.NewError(connect.CodePermissionDenied, errors.New("only the control center may call this"))
	}
	return c, nil
}

// watcher checks that the caller may watch presence and approvals: the
// control center, or the operator (read-only, to roll idle rooms).
func watcher(ctx context.Context) error {
	if c, _ := CallerFrom(ctx); c.Operator {
		return nil
	}
	_, err := controlCenter(ctx)
	return err
}

func unavailable(err error) error {
	if ce := new(connect.Error); errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeUnavailable, err)
}

// secretRefusal refuses a body that carries one of the workspace's issued
// tokens (design §11.4). It is refused with the body left out of the audit,
// so the token reaches no record.
var secretRefusal = &policy.Refusal{Code: policy.Invalid, Rule: "secret",
	Reason: "this message contains a secret token; it was not sent. Send it again without the token"}

var refusalCodes = map[policy.Code]connect.Code{
	policy.Denied:    connect.CodePermissionDenied,
	policy.Invalid:   connect.CodeInvalidArgument,
	policy.NotFound:  connect.CodeNotFound,
	policy.Exhausted: connect.CodeResourceExhausted,
}

// refused audits a refusal and returns it as a Connect error.
func (s *Server) refused(ctx context.Context, from *party, to, id, body string, r *policy.Refusal) error {
	s.auditRefusal(ctx, from, to, id, body, r)
	return connect.NewError(refusalCodes[r.Code], r)
}

func (s *Server) auditRefusal(ctx context.Context, from *party, to, id, body string, r *policy.Refusal) {
	rec := &messagingv1.AuditRecord{Kind: store.AuditRefusal, From: from.addr, To: to, MessageId: id,
		Reason: r.Rule + ": " + r.Reason}
	if from.ws.AuditBodies() {
		rec.Body = body
	}
	if err := s.st.Audit(ctx, from.ws.Name, rec); err != nil {
		s.log.Warn("audit refusal", "err", err)
	}
	s.log.Info("refused", "from", from.addr, "to", to, "rule", r.Rule, "reason", r.Reason)
}

// Send implements MessagingService.
func (s *Server) Send(ctx context.Context, req *connect.Request[messagingv1.SendRequest]) (*connect.Response[messagingv1.SendResponse], error) {
	from, err := s.sender(ctx, policy.Forge)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.Id == "" || len(m.Id) > 128 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required (a ULID, at most 128 characters)"))
	}
	if from.ws.TokenHashes.Contains(m.Body) {
		return nil, s.refused(ctx, from, m.To, m.Id, "", secretRefusal)
	}
	replyTo := ""
	if m.ReplyTo != "" {
		t, err := policy.ParseAddress(m.ReplyTo, from.ws.Name)
		if err != nil || t.Group() {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("reply_to must be name or name@workspace: %q", m.ReplyTo))
		}
		replyTo = t.String()
	}
	rate, key := from.ws.RatePerMinute(), from.addr
	if from.p.Kind == policy.System {
		rate = policy.ForgeRatePerMinute
	}
	if r := s.lim.Allow(key, rate, time.Now()); r != nil {
		return nil, s.refused(ctx, from, m.To, m.Id, m.Body, r)
	}
	route, r := policy.Check(from.ws, s.isPerson(ctx, from.ws.Name), from.p, m.To, m.Body)
	if r != nil {
		return nil, s.refused(ctx, from, m.To, m.Id, m.Body, r)
	}
	if route.Group {
		// Group sends count against the sender's fan-out budget (§7.8).
		for _, sk := range route.Skipped {
			s.auditRefusal(ctx, from, policy.Address(sk.Name, from.ws.Name), m.Id, m.Body, sk.Refusal)
		}
		if n, per := len(route.Recipients), from.ws.FanoutPerMinute(); !s.lim.AllowN("fanout/"+from.addr, n, per, time.Now()) {
			return nil, s.refused(ctx, from, m.To, m.Id, m.Body, &policy.Refusal{Code: policy.Exhausted, Rule: "fanout",
				Reason: fmt.Sprintf("%s reaches %d recipients, over %s's fan-out budget of %d a minute; wait, or post to a channel instead",
					m.To, n, from.addr, per)})
		}
	}
	if t, err := policy.ParseAddress(m.To, from.ws.Name); err == nil {
		if r := s.sameBody(ctx, from, t.String(), m.Body, m.Id); r != nil {
			return nil, s.refused(ctx, from, m.To, m.Id, m.Body, r)
		}
	}
	sentAt := timestamppb.Now()
	allDup := true
	for _, rcpt := range route.Recipients {
		to := policy.Address(rcpt.Name, from.ws.Name)
		msg := &messagingv1.Message{Id: m.Id, From: from.addr, To: to, ReplyTo: replyTo, Body: m.Body,
			SentAt: sentAt, FromKind: string(from.p.Kind)}
		dup, err := s.st.Deliver(ctx, from.ws.Name, rcpt.Name, m.Id+"/"+rcpt.Name,
			&messagingv1.Delivery{Urgent: from.p.Kind == policy.Human, Item: &messagingv1.Delivery_Message{Message: msg}})
		if err != nil {
			return nil, unavailable(err)
		}
		if dup {
			continue
		}
		allDup = false
		s.auditMessage(ctx, from, to, m, route.LinkTo(rcpt.Name), route.Mediated)
	}
	return connect.NewResponse(&messagingv1.SendResponse{Id: m.Id, Duplicate: allDup}), nil
}

func (s *Server) auditMessage(ctx context.Context, from *party, to string, m *messagingv1.SendRequest, link string, mediated bool) {
	rec := &messagingv1.AuditRecord{Kind: store.AuditMessage, From: from.addr, To: to, MessageId: m.Id, Link: link}
	if from.ws.AuditBodies() {
		rec.Body = m.Body
	}
	if err := s.st.Audit(ctx, from.ws.Name, rec); err != nil {
		s.log.Warn("audit message", "err", err)
	}
	if mediated {
		// Every crossing of a mediated link is logged in full (§7.4).
		cr := &messagingv1.AuditRecord{Kind: store.AuditCrossing, From: from.addr, To: to, MessageId: m.Id,
			Link: link, Body: m.Body}
		if err := s.st.Audit(ctx, from.ws.Name, cr); err != nil {
			s.log.Warn("audit crossing", "err", err)
		}
	}
}

func (s *Server) isPerson(ctx context.Context, ws string) policy.IsPerson {
	return func(name string) bool { return s.st.IsPerson(ctx, ws, name) }
}

// Subscribe implements MessagingService.
func (s *Server) Subscribe(ctx context.Context, req *connect.Request[messagingv1.SubscribeRequest], stream *connect.ServerStream[messagingv1.Delivery]) error {
	me, err := s.sender(ctx, policy.Status)
	if err != nil {
		return err
	}
	if err := stream.Send(nil); err != nil { // send headers now, so clients see the stream open
		return err
	}
	err = s.st.Subscribe(ctx, me.ws.Name, me.p.Name, s.budget(me, stream.Send))
	if err != nil && ctx.Err() == nil {
		return unavailable(err)
	}
	return nil
}

// Ack implements MessagingService.
func (s *Server) Ack(ctx context.Context, req *connect.Request[messagingv1.AckRequest]) (*connect.Response[messagingv1.AckResponse], error) {
	me, err := s.sender(ctx, policy.Status)
	if err != nil {
		return nil, err
	}
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.st.Ack(actx, me.ws.Name, me.p.Name, req.Msg.DeliveryId); err != nil {
		if errors.Is(err, store.ErrBadDelivery) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("delivery_id is not a delivery of %s's inbox", me.addr))
		}
		return nil, unavailable(err)
	}
	s.markPushedRead(actx, me, req.Msg.DeliveryId)
	return connect.NewResponse(&messagingv1.AckResponse{}), nil
}

// ReportPresence implements MessagingService.
func (s *Server) ReportPresence(ctx context.Context, req *connect.Request[messagingv1.ReportPresenceRequest]) (*connect.Response[messagingv1.ReportPresenceResponse], error) {
	me, err := s.room(ctx)
	if err != nil {
		return nil, err
	}
	p := req.Msg.Presence
	if p == messagingv1.Presence_PRESENCE_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("presence is required"))
	}
	prev, err := s.st.PutPresence(ctx, me.ws.Name, me.p.Name, p)
	if err != nil {
		return nil, unavailable(err)
	}
	if prev != p {
		rec := &messagingv1.AuditRecord{Kind: store.AuditPresence, From: me.addr, Reason: prev.String() + " -> " + p.String()}
		if err := s.st.Audit(ctx, me.ws.Name, rec); err != nil {
			s.log.Warn("audit presence", "err", err)
		}
	}
	return connect.NewResponse(&messagingv1.ReportPresenceResponse{}), nil
}

// ReportStatus implements MessagingService.
func (s *Server) ReportStatus(ctx context.Context, req *connect.Request[messagingv1.ReportStatusRequest]) (*connect.Response[messagingv1.ReportStatusResponse], error) {
	me, err := s.room(ctx)
	if err != nil {
		return nil, err
	}
	if req.Msg.Status == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("status is required"))
	}
	if err := s.st.PutStatus(ctx, me.ws.Name, me.p.Name, req.Msg.Status); err != nil {
		return nil, unavailable(err)
	}
	return connect.NewResponse(&messagingv1.ReportStatusResponse{}), nil
}

// ListPeers implements MessagingService.
func (s *Server) ListPeers(ctx context.Context, req *connect.Request[messagingv1.ListPeersRequest]) (*connect.Response[messagingv1.ListPeersResponse], error) {
	me, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	out := &messagingv1.ListPeersResponse{Self: me.addr}
	for _, name := range me.ws.RoomNames() {
		r := me.ws.Rooms[name]
		to := policy.Party{Name: name, Kind: policy.Agent, Group: r.Group}
		ok, note := policy.Allowed(me.ws, me.p, to)
		if !ok {
			continue
		}
		out.Peers = append(out.Peers, &messagingv1.Peer{
			Address: policy.Address(name, me.ws.Name), Kind: string(policy.Agent), DisplayName: r.DisplayName,
			Title: r.Title, Summary: r.Summary, Group: r.Group, Note: note,
			Presence:    s.st.Presence(ctx, me.ws.Name, name),
			CurrentTask: s.st.Status(ctx, me.ws.Name, name).GetCurrentTask(),
		})
	}
	if ok, _ := policy.Allowed(me.ws, me.p, policy.Party{Name: policy.Status, Kind: policy.System}); ok {
		out.Peers = append(out.Peers, &messagingv1.Peer{Address: policy.Address(policy.Status, me.ws.Name),
			Kind: string(policy.System), DisplayName: "Swarm status",
			Note: "status reports for the control center; one-way"})
	}
	people, err := s.st.People(ctx, me.ws.Name)
	if err != nil {
		return nil, unavailable(err)
	}
	slices.Sort(people)
	for _, name := range people {
		if _, isRoom := me.ws.Rooms[name]; isRoom {
			continue
		}
		if ok, note := policy.Allowed(me.ws, me.p, policy.Party{Name: name, Kind: policy.Human}); ok {
			out.Peers = append(out.Peers, &messagingv1.Peer{Address: policy.Address(name, me.ws.Name),
				Kind: string(policy.Human), DisplayName: name, Note: note})
		}
	}
	return connect.NewResponse(out), nil
}

// QueryAudit implements MessagingService.
func (s *Server) QueryAudit(ctx context.Context, req *connect.Request[messagingv1.QueryAuditRequest], stream *connect.ServerStream[messagingv1.AuditRecord]) error {
	if _, err := controlCenter(ctx); err != nil {
		return err
	}
	m := req.Msg
	if !policy.ValidName(m.Workspace) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("workspace is required"))
	}
	for _, k := range m.Kinds {
		if !slices.Contains([]string{store.AuditMessage, store.AuditCrossing, store.AuditRefusal, store.AuditPresence, store.AuditVerdict, store.AuditPost}, k) {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown audit kind %q: use message, post, crossing, refusal, presence or verdict", k))
		}
	}
	q := store.AuditQuery{Workspace: m.Workspace, Kinds: m.Kinds, Address: m.Address, Limit: int(m.Limit)}
	if m.Since != nil {
		q.Since = m.Since.AsTime()
	}
	if m.Until != nil {
		q.Until = m.Until.AsTime()
	}
	if err := s.st.QueryAudit(ctx, q, stream.Send); err != nil && ctx.Err() == nil {
		return unavailable(err)
	}
	return nil
}
