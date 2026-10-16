package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
	"github.com/usr-lex/swarmageddon/internal/messaging/policy"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
)

// channel looks up a channel and resolves its members.
func (s *Server) channel(ctx context.Context, ws *directory.Workspace, name string) (*v1alpha1.ChannelSpec, *directory.ChannelMembers, error) {
	name = strings.TrimPrefix(name, "#")
	ch := ws.Channel(name)
	if ch == nil {
		return nil, nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("there is no channel #%s in workspace %s; list_channels shows yours", name, ws.Name))
	}
	people, err := s.st.People(ctx, ws.Name)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	return ch, ws.ResolveChannel(ch, people), nil
}

// deliveryMode is a reader's mode for a channel: their choice if the channel
// still allows it, else the channel's default.
func (s *Server) deliveryMode(ctx context.Context, ws, member string, ch *v1alpha1.ChannelSpec) v1alpha1.DeliveryMode {
	mode, err := s.st.Delivery(ctx, ws, member, ch.Name)
	if err == nil && mode != "" && slices.Contains(directory.AllowedDelivery(ch), v1alpha1.DeliveryMode(mode)) {
		return v1alpha1.DeliveryMode(mode)
	}
	return directory.DefaultDelivery(ch)
}

// mentions returns the names mentioned with @name in body, in order, once
// each. @role:x and the like are not mentions, nor is the @ of an address.
func mentions(body string) []string {
	var out []string
	for i := 0; i < len(body); i++ {
		if body[i] != '@' || (i > 0 && isNameByte(body[i-1])) || (i > 0 && body[i-1] == '@') {
			continue
		}
		j := i + 1
		for j < len(body) && isNameByte(body[j]) {
			j++
		}
		if j == i+1 || (j < len(body) && (body[j] == ':' || body[j] == '@' || (body[j] >= 'A' && body[j] <= 'Z'))) {
			continue
		}
		if name := body[i+1 : j]; policy.ValidName(name) && !slices.Contains(out, name) {
			out = append(out, name)
		}
		i = j - 1
	}
	return out
}

func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-' || b == '.'
}

// Post implements MessagingService: a post to a channel (docs/design.md §7.8).
func (s *Server) Post(ctx context.Context, req *connect.Request[messagingv1.PostRequest]) (*connect.Response[messagingv1.PostResponse], error) {
	from, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.Id == "" || len(m.Id) > 128 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required (a ULID, at most 128 characters)"))
	}
	name := strings.TrimPrefix(m.Channel, "#")
	target := "#" + name
	ch, mem, err := s.channel(ctx, from.ws, name)
	if err != nil {
		return nil, err
	}
	ws := from.ws
	acc := mem.Access[from.p.Name]
	switch {
	case !acc.Write && !acc.Read:
		return nil, s.refused(ctx, from, target, m.Id, m.Body, &policy.Refusal{Code: policy.Denied, Rule: "channel-access",
			Reason: fmt.Sprintf("%s is not a member of %s; list_channels shows the channels you can post to", from.addr, target)})
	case !acc.Write:
		return nil, s.refused(ctx, from, target, m.Id, m.Body, &policy.Refusal{Code: policy.Denied, Rule: "channel-access",
			Reason: fmt.Sprintf("%s may read %s but not post to it", from.addr, target)})
	}
	if strings.TrimSpace(m.Body) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("body is empty"))
	}
	if len(m.Body) > ws.MaxBytes() {
		return nil, s.refused(ctx, from, target, m.Id, m.Body, &policy.Refusal{Code: policy.Invalid, Rule: "size",
			Reason: fmt.Sprintf("the post is %d bytes; the limit is %d", len(m.Body), ws.MaxBytes())})
	}
	now := time.Now()
	if r := s.lim.Allow(from.addr, ws.RatePerMinute(), now); r != nil {
		return nil, s.refused(ctx, from, target, m.Id, m.Body, r)
	}
	if per := directory.PostsPerMinute(ch); !s.lim.AllowN("chan/"+ws.Name+"/"+name, 1, per, now) {
		return nil, s.refused(ctx, from, target, m.Id, m.Body, &policy.Refusal{Code: policy.Exhausted, Rule: "channel-rate",
			Reason: fmt.Sprintf("%s takes at most %d posts a minute; wait and retry", target, per)})
	}
	if r := s.sameBody(ctx, from, target, m.Body, m.Id); r != nil {
		return nil, s.refused(ctx, from, target, m.Id, m.Body, r)
	}

	var mentioned []string
	for _, n := range mentions(m.Body) {
		if n != from.p.Name && mem.Access[n].Read {
			mentioned = append(mentioned, n)
		}
	}
	post := &messagingv1.ChannelPost{Id: m.Id, Channel: name, From: from.addr, FromKind: string(from.p.Kind),
		Body: m.Body, SentAt: timestamppb.New(now)}
	for _, n := range mentioned {
		post.Mentions = append(post.Mentions, policy.Address(n, ws.Name))
	}
	seq, dup, err := s.st.PublishPost(ctx, ws.Name, post)
	if err != nil {
		return nil, unavailable(err)
	}
	if dup {
		// A retry: push again with the original, in case the first try
		// stopped half way; inbox ids make this idempotent.
		if e, err := s.st.Get(ctx, store.ChannelStream(ws.Name), seq); err == nil && e != nil {
			var orig messagingv1.ChannelPost
			if proto.Unmarshal(e.Data, &orig) == nil {
				post = &orig
			}
		}
	}
	for _, r := range mem.Readers() {
		if r == from.p.Name {
			continue
		}
		if !slices.Contains(mentioned, r) && s.deliveryMode(ctx, ws.Name, r, ch) != v1alpha1.DeliveryPush {
			continue
		}
		d := &messagingv1.Delivery{Item: &messagingv1.Delivery_Post{Post: post}}
		if _, err := s.st.DeliverPost(ctx, ws.Name, r, "post/"+m.Id+"/"+r, d, seq); err != nil {
			return nil, unavailable(err)
		}
	}
	if !dup {
		rec := &messagingv1.AuditRecord{Kind: store.AuditPost, From: from.addr, To: target, MessageId: m.Id}
		if ws.AuditBodies() {
			rec.Body = m.Body
		}
		if err := s.st.Audit(ctx, ws.Name, rec); err != nil {
			s.log.Warn("audit post", "err", err)
		}
	}
	return connect.NewResponse(&messagingv1.PostResponse{Id: m.Id, Duplicate: dup, Mentioned: post.Mentions}), nil
}

// sameBody refuses an agent's second, different message with the same body
// to the same target within the dedup window. People are not limited.
func (s *Server) sameBody(ctx context.Context, from *party, target, body, id string) *policy.Refusal {
	if from.p.Kind != policy.Agent {
		return nil
	}
	prev, err := s.st.SameBody(ctx, from.ws.Name, from.p.Name, target, body, id)
	if err != nil {
		s.log.Warn("duplicate check", "err", err)
		return nil
	}
	if prev == "" {
		return nil
	}
	return &policy.Refusal{Code: policy.Invalid, Rule: "duplicate", Reason: fmt.Sprintf(
		"you already sent this exact text to %s in the last %s (message %s); don't repeat it, or say something new",
		target, store.DedupWindow, prev)}
}

// ListChannels implements MessagingService.
func (s *Server) ListChannels(ctx context.Context, req *connect.Request[messagingv1.ListChannelsRequest]) (*connect.Response[messagingv1.ListChannelsResponse], error) {
	me, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	counts, err := s.unreadCounts(ctx, me)
	if err != nil {
		return nil, err
	}
	out := &messagingv1.ListChannelsResponse{}
	for i := range me.ws.Messaging.Channels {
		ch := &me.ws.Messaging.Channels[i]
		_, mem, err := s.channel(ctx, me.ws, ch.Name)
		if err != nil {
			return nil, err
		}
		acc := mem.Access[me.p.Name]
		if acc == (directory.Access{}) {
			continue
		}
		c := &messagingv1.Channel{Name: ch.Name, Description: ch.Description, Access: acc.String(),
			Unread: int32(counts[ch.Name]), Members: int32(len(mem.Access))}
		if acc.Read {
			c.Delivery = string(s.deliveryMode(ctx, me.ws.Name, me.p.Name, ch))
			for _, m := range directory.AllowedDelivery(ch) {
				c.AllowedDelivery = append(c.AllowedDelivery, string(m))
			}
		}
		out.Channels = append(out.Channels, c)
	}
	return connect.NewResponse(out), nil
}

// SetDelivery implements MessagingService.
func (s *Server) SetDelivery(ctx context.Context, req *connect.Request[messagingv1.SetDeliveryRequest]) (*connect.Response[messagingv1.SetDeliveryResponse], error) {
	me, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	ch, mem, err := s.channel(ctx, me.ws, req.Msg.Channel)
	if err != nil {
		return nil, err
	}
	if !mem.Access[me.p.Name].Read {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s doesn't read #%s, so it has no delivery mode", me.addr, ch.Name))
	}
	mode := v1alpha1.DeliveryMode(req.Msg.Mode)
	allowed := directory.AllowedDelivery(ch)
	if !slices.Contains(allowed, mode) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("#%s allows delivery %v, not %q", ch.Name, allowed, req.Msg.Mode))
	}
	if err := s.st.SetDelivery(ctx, me.ws.Name, me.p.Name, ch.Name, string(mode)); err != nil {
		return nil, unavailable(err)
	}
	return connect.NewResponse(&messagingv1.SetDeliveryResponse{}), nil
}

// digestPreviews is how many first lines a digest carries.
const digestPreviews = 3

// runDigests sends due digests for every workspace whose inbox stream this
// replica leads. State lives in KV, so a new leader carries on, and inbox
// ids make a digest sent twice across a failover arrive once.
func (s *Server) runDigests(ctx context.Context) {
	for _, name := range s.dir.Workspaces() {
		ws, ok := s.dir.Workspace(name)
		if !ok || len(ws.Messaging.Channels) == 0 || !s.st.IsInboxLeader(ctx, name) {
			continue
		}
		for i := range ws.Messaging.Channels {
			if err := s.digestChannel(ctx, ws, &ws.Messaging.Channels[i]); err != nil && ctx.Err() == nil {
				s.log.Warn("digest", "workspace", name, "channel", ws.Messaging.Channels[i].Name, "err", err)
			}
		}
	}
}

func (s *Server) digestChannel(ctx context.Context, ws *directory.Workspace, ch *v1alpha1.ChannelSpec) error {
	subject := store.ChannelSubject(ws.Name, ch.Name)
	last, err := s.st.LastSeqOf(ctx, store.ChannelStream(ws.Name), subject)
	if err != nil || last == 0 {
		return err
	}
	people, err := s.st.People(ctx, ws.Name)
	if err != nil {
		return err
	}
	mem := ws.ResolveChannel(ch, people)
	for _, reader := range mem.Readers() {
		if s.deliveryMode(ctx, ws.Name, reader, ch) != v1alpha1.DeliveryDigest {
			continue
		}
		ds, err := s.st.Digest(ctx, ws.Name, reader, ch.Name)
		if err != nil {
			return err
		}
		if ds.Seq >= last || time.Since(ds.Sent) < ws.DigestInterval() {
			continue
		}
		rs, err := s.st.ReadState(ctx, ws.Name, reader, ch.Name)
		if err != nil {
			return err
		}
		me := policy.Address(reader, ws.Name)
		dg := &messagingv1.Digest{Channel: ch.Name}
		upto := ds.Seq
		err = s.st.Scan(ctx, store.ChannelStream(ws.Name), []string{subject}, max(ds.Seq, rs.Upto)+1, func(e *store.Entry) bool {
			upto = e.Seq
			var p messagingv1.ChannelPost
			if proto.Unmarshal(e.Data, &p) != nil || p.From == me || rs.Read(e.Seq) {
				return true
			}
			dg.NewPosts++
			if !slices.Contains(dg.Posters, p.From) {
				dg.Posters = append(dg.Posters, p.From)
			}
			if len(dg.Previews) < digestPreviews {
				dg.Previews = append(dg.Previews, firstLine(p.Body, 120))
			}
			return true
		})
		if err != nil {
			return err
		}
		next := store.DigestState{Seq: upto, Sent: ds.Sent}
		if dg.NewPosts > 0 {
			d := &messagingv1.Delivery{Item: &messagingv1.Delivery_Digest{Digest: dg}}
			id := fmt.Sprintf("digest/%s/%s/%d", ch.Name, reader, upto)
			if _, err := s.st.Deliver(ctx, ws.Name, reader, id, d); err != nil {
				return err
			}
			next.Sent = time.Now()
			s.log.Debug("digest sent", "to", me, "channel", ch.Name, "posts", dg.NewPosts, "upto", upto)
		}
		if err := s.st.SetDigest(ctx, ws.Name, reader, ch.Name, next); err != nil {
			return err
		}
	}
	return nil
}

// firstLine is the first non-empty line of s, at most n runes.
func firstLine(s string, n int) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if r := []rune(l); len(r) > n {
				return string(r[:n]) + "…"
			}
			return l
		}
	}
	return ""
}
