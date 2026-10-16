package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/msgclient"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// ---------------------------------------------------------------- channels

type channelSvc struct{ *Server }

// as checks the person may see the workspace and returns their address.
func (s channelSvc) as(ctx context.Context, ws string) (string, error) {
	p, err := s.canView(ctx, ws)
	if err != nil {
		return "", err
	}
	if s.Msg == nil {
		return "", connect.NewError(connect.CodeUnavailable, errors.New("messaging is not connected"))
	}
	return p.Handle + "@" + ws, nil
}

func onBehalf[T any](msg *T, as string) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set(msgclient.OnBehalfHeader, as)
	return r
}

func itemProto(i *messagingv1.InboxItem) *ccv1.ChannelItem {
	return &ccv1.ChannelItem{Id: i.GetId(), Channel: i.GetChannel(), From: i.GetFrom(), FromKind: i.GetFromKind(), Body: i.GetBody(),
		SentAt: i.GetSentAt(), Truncated: i.GetTruncated(), Read: i.GetRead(), MentionsMe: i.GetMentionsMe()}
}

func (s channelSvc) ListChannels(ctx context.Context, req *connect.Request[ccv1.ListChannelsRequest]) (*connect.Response[ccv1.ListChannelsResponse], error) {
	as, err := s.as(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	res, err := s.Msg.ListChannels(ctx, onBehalf(&messagingv1.ListChannelsRequest{}, as))
	if err != nil {
		return nil, err
	}
	out := &ccv1.ListChannelsResponse{}
	for _, c := range res.Msg.GetChannels() {
		out.Channels = append(out.Channels, &ccv1.ChannelInfo{Name: c.GetName(), Description: c.GetDescription(), Access: c.GetAccess(),
			Delivery: c.GetDelivery(), AllowedDelivery: c.GetAllowedDelivery(), Unread: c.GetUnread(), Members: c.GetMembers()})
	}
	return connect.NewResponse(out), nil
}

// PostToChannel posts as the person. Viewers are read-only (§8.1), so only
// operators of the workspace (and admins) may post, to any channel:
// messaging skips its membership check for them (x-swarm-operator-post) but
// still applies limits and audits the post.
func (s channelSvc) PostToChannel(ctx context.Context, req *connect.Request[ccv1.PostToChannelRequest]) (*connect.Response[ccv1.PostToChannelResponse], error) {
	ws := req.Msg.GetWorkspace()
	if _, err := s.canOperate(ctx, ws); err != nil {
		return nil, err
	}
	as, err := s.as(ctx, ws)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(req.Msg.GetBody())
	if body == "" {
		return nil, errInvalid("the post is empty")
	}
	ch := strings.TrimPrefix(req.Msg.GetChannel(), "#")
	id := ulid.Make().String()
	// Operators may post to any channel of their workspace (§7.8); this
	// header tells messaging the person passed canOperate above.
	preq := onBehalf(&messagingv1.PostRequest{Id: id, Channel: ch, Body: body}, as)
	preq.Header().Set("x-swarm-operator-post", "1")
	res, err := s.Msg.Post(ctx, preq)
	s.audit(ctx, req, "channel.post", "channel/"+ch, ws, nil, map[string]any{"id": id, "bytes": len(body)}, err)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.PostToChannelResponse{Id: res.Msg.GetId(), Mentioned: res.Msg.GetMentioned()}), nil
}

func (s channelSvc) ReadChannelMessages(ctx context.Context, req *connect.Request[ccv1.ReadChannelMessagesRequest]) (*connect.Response[ccv1.ReadChannelMessagesResponse], error) {
	m := req.Msg
	as, err := s.as(ctx, m.GetWorkspace())
	if err != nil {
		return nil, err
	}
	res, err := s.Msg.ReadMessages(ctx, onBehalf(&messagingv1.ReadMessagesRequest{Scope: m.GetScope(), UnreadOnly: m.UnreadOnly,
		Limit: m.GetLimit(), Cursor: m.GetCursor(), Full: m.GetFull(), MarkRead: m.MarkRead}, as))
	if err != nil {
		return nil, err
	}
	out := &ccv1.ReadChannelMessagesResponse{NextCursor: res.Msg.GetNextCursor(), UnreadRemaining: res.Msg.GetUnreadRemaining()}
	for _, i := range res.Msg.GetItems() {
		out.Items = append(out.Items, itemProto(i))
	}
	return connect.NewResponse(out), nil
}

func (s channelSvc) GetChannelMessage(ctx context.Context, req *connect.Request[ccv1.GetChannelMessageRequest]) (*connect.Response[ccv1.GetChannelMessageResponse], error) {
	as, err := s.as(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	res, err := s.Msg.GetMessage(ctx, onBehalf(&messagingv1.GetMessageRequest{Id: req.Msg.GetId()}, as))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.GetChannelMessageResponse{Item: itemProto(res.Msg.GetItem())}), nil
}

func (s channelSvc) MarkChannelRead(ctx context.Context, req *connect.Request[ccv1.MarkChannelReadRequest]) (*connect.Response[ccv1.MarkChannelReadResponse], error) {
	as, err := s.as(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	if _, err := s.Msg.MarkRead(ctx, onBehalf(&messagingv1.MarkReadRequest{Scope: req.Msg.GetScope(), UpTo: req.Msg.GetUpTo()}, as)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.MarkChannelReadResponse{}), nil
}

func (s channelSvc) GetUnreadSummary(ctx context.Context, req *connect.Request[ccv1.GetUnreadSummaryRequest]) (*connect.Response[ccv1.GetUnreadSummaryResponse], error) {
	as, err := s.as(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	res, err := s.Msg.UnreadSummary(ctx, onBehalf(&messagingv1.UnreadSummaryRequest{}, as))
	if err != nil {
		return nil, err
	}
	out := &ccv1.GetUnreadSummaryResponse{}
	for _, c := range res.Msg.GetCounts() {
		out.Counts = append(out.Counts, &ccv1.UnreadScope{Scope: c.GetScope(), Unread: c.GetUnread()})
	}
	return connect.NewResponse(out), nil
}

func (s channelSvc) SetChannelDelivery(ctx context.Context, req *connect.Request[ccv1.SetChannelDeliveryRequest]) (*connect.Response[ccv1.SetChannelDeliveryResponse], error) {
	as, err := s.as(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	if !slices.Contains([]string{"push", "digest", "pull"}, req.Msg.GetMode()) {
		return nil, errInvalid("mode must be push, digest or pull")
	}
	if _, err := s.Msg.SetDelivery(ctx, onBehalf(&messagingv1.SetDeliveryRequest{Channel: strings.TrimPrefix(req.Msg.GetChannel(), "#"), Mode: req.Msg.GetMode()}, as)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.SetChannelDeliveryResponse{}), nil
}

// ---------------------------------------------------------------- warden

// VerdictHub fans new and decided verdicts out to watchers.
type VerdictHub struct {
	mu   sync.Mutex
	subs map[chan store.Verdict]struct{}
}

func (h *VerdictHub) publish(v store.Verdict) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

func (h *VerdictHub) subscribe() (<-chan store.Verdict, func()) {
	ch := make(chan store.Verdict, 64)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan store.Verdict]struct{}{}
	}
	h.subs[ch] = struct{}{}
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

// OnVerdict stores a verdict from the messaging audit, notifies people of
// high and critical ones (wardenFlag), and updates the fleet badges.
func (s *Server) OnVerdict(ctx context.Context, ws string, v *messagingv1.Verdict) {
	db := s.DB()
	if db == nil {
		return
	}
	room, _, _ := strings.Cut(v.GetRoom(), "@")
	at := time.Now().UTC()
	if v.GetAt() != nil {
		at = v.GetAt().AsTime()
	}
	sv := store.Verdict{ID: v.GetId(), Workspace: ws, Room: room, Layer: v.GetLayer(), Severity: int(v.GetSeverity()), Rule: v.GetRule(),
		Reason: v.GetReason(), Evidence: v.GetEvidence(), Action: v.GetAction(), Source: v.GetSource(), At: at}
	isNew, err := db.InsertVerdict(ctx, sv)
	if err != nil || !isNew {
		return
	}
	s.Verdicts.publish(sv)
	if v.GetSeverity() >= messagingv1.Severity_SEVERITY_HIGH {
		reason := sv.Reason
		if reason == "" {
			reason = sv.Rule
		}
		s.Notify.Raise(ctx, notify.Event{Kind: v1alpha1.EventWardenFlag, Workspace: ws, Room: room, Fingerprint: sv.ID,
			Title: room + ": warden flagged " + severityWord(sv.Severity), Body: sv.Layer + ": " + reason + " (" + sv.Action + ")",
			URL: "/workspaces/" + ws + "/rooms/" + room + "/activity"})
	}
	s.Fleet.Kick()
}

func severityWord(s int) string {
	switch s {
	case 1:
		return "low"
	case 2:
		return "medium"
	case 3:
		return "high"
	case 4:
		return "critical"
	}
	return "unknown"
}

type wardenSvc struct{ *Server }

func verdictProto(v store.Verdict) *ccv1.Verdict {
	return &ccv1.Verdict{Id: v.ID, Workspace: v.Workspace, Room: v.Room, Layer: v.Layer, Severity: ccv1.Severity(v.Severity), Rule: v.Rule,
		Reason: v.Reason, Evidence: v.Evidence, Action: v.Action, Source: v.Source, At: ts(v.At), Decision: v.Decision,
		DecidedBy: v.DecidedBy, DecidedAt: tsp(v.DecidedAt), Note: v.Note}
}

func (s wardenSvc) ListVerdicts(ctx context.Context, req *connect.Request[ccv1.ListVerdictsRequest]) (*connect.Response[ccv1.ListVerdictsResponse], error) {
	m := req.Msg
	if _, err := s.canView(ctx, m.GetWorkspace()); err != nil {
		return nil, err
	}
	db := s.DB()
	if db == nil {
		return nil, errNoDB()
	}
	var before time.Time
	if m.GetBefore() != nil {
		before = m.GetBefore().AsTime()
	}
	vs, err := db.ListVerdicts(ctx, store.VerdictFilter{Workspace: m.GetWorkspace(), Room: m.GetRoom(), UndecidedOnly: m.GetUndecidedOnly(),
		MinSeverity: int(m.GetMinSeverity()), Before: before, Limit: int(m.GetLimit())})
	if err != nil {
		return nil, err
	}
	out := &ccv1.ListVerdictsResponse{}
	for _, v := range vs {
		out.Verdicts = append(out.Verdicts, verdictProto(v))
	}
	return connect.NewResponse(out), nil
}

func (s wardenSvc) DecideVerdict(ctx context.Context, req *connect.Request[ccv1.DecideVerdictRequest]) (*connect.Response[ccv1.DecideVerdictResponse], error) {
	db := s.DB()
	if db == nil {
		return nil, errNoDB()
	}
	if d := req.Msg.GetDecision(); d != "dismiss" && d != "confirm" {
		return nil, errInvalid("decision must be dismiss or confirm")
	}
	v, err := db.GetVerdict(ctx, req.Msg.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	p, err := s.canOperate(ctx, v.Workspace)
	if err != nil {
		return nil, err
	}
	nv, err := db.DecideVerdict(ctx, v.ID, req.Msg.GetDecision(), p.Name(), req.Msg.GetNote())
	s.audit(ctx, req, "verdict."+req.Msg.GetDecision(), "verdict/"+v.ID, v.Workspace, v.Decision, req.Msg.GetDecision(), err)
	if err != nil {
		return nil, err
	}
	s.Verdicts.publish(nv)
	s.Fleet.Kick()
	return connect.NewResponse(&ccv1.DecideVerdictResponse{Verdict: verdictProto(nv)}), nil
}

func (s wardenSvc) WatchVerdicts(ctx context.Context, req *connect.Request[ccv1.WatchVerdictsRequest], stream *connect.ServerStream[ccv1.WatchVerdictsResponse]) error {
	p, err := signedIn(ctx)
	if err != nil {
		return err
	}
	allowed, want := s.viewFilter(ctx, p), wantSet(req.Msg.GetWorkspaces())
	ch, cancel := s.Verdicts.subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case v, ok := <-ch:
			if !ok {
				return nil
			}
			if !allowed(v.Workspace) || !want(v.Workspace) {
				continue
			}
			if err := stream.Send(&ccv1.WatchVerdictsResponse{Verdict: verdictProto(v)}); err != nil {
				return err
			}
		}
	}
}
