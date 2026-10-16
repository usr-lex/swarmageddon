package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
)

// Reading (docs/design.md §7.8): direct messages and readable channel posts
// in one paginated view, with a read state per member and scope.
const (
	defaultPage  = 20
	maxPage      = 50
	previewRunes = 500

	scopeAll      = "all"
	scopeDirect   = "direct"
	scopeMentions = "mentions"
)

// source is one place items come from: the member's direct messages (their
// inbox subject) or one channel. key is its read-state scope.
type source struct {
	key, stream, subject string
	channel              string // "" for direct messages
	order                int
}

// item is one direct message or post in a source.
type item struct {
	src  *source
	seq  uint64
	at   time.Time
	it   *messagingv1.InboxItem
	mine bool // posted by the reader: never unread
}

// scanned is a source read from sequence from on.
type scanned struct {
	src   *source
	rs    store.ReadState
	from  uint64
	seqs  []uint64        // every sequence read
	items map[uint64]bool // sequences that are unread-able items
	list  []*item         // the items keep accepted
}

func directSource(me *party) *source {
	return &source{key: scopeDirect, stream: store.InboxStream(me.ws.Name), subject: store.InboxSubject(me.ws.Name, me.p.Name)}
}

// readable lists the channels the member reads, as sources.
func (s *Server) readable(ctx context.Context, me *party) ([]*source, error) {
	people, err := s.st.People(ctx, me.ws.Name)
	if err != nil {
		return nil, unavailable(err)
	}
	var out []*source
	for i := range me.ws.Messaging.Channels {
		ch := &me.ws.Messaging.Channels[i]
		if me.ws.ResolveChannel(ch, people).Access[me.p.Name].Read {
			out = append(out, &source{key: ch.Name, channel: ch.Name, stream: store.ChannelStream(me.ws.Name),
				subject: store.ChannelSubject(me.ws.Name, ch.Name)})
		}
	}
	return out, nil
}

// scope resolves a reading scope: all, direct, mentions or a channel.
// mentionsOnly keeps only posts that mention the member.
func (s *Server) scope(ctx context.Context, me *party, scope string) (srcs []*source, mentionsOnly bool, err error) {
	switch scope {
	case "", scopeAll, scopeDirect, scopeMentions:
	default:
		ch, mem, err := s.channel(ctx, me.ws, scope)
		if err != nil {
			return nil, false, err
		}
		if !mem.Access[me.p.Name].Read {
			return nil, false, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s can't read #%s", me.addr, ch.Name))
		}
		return []*source{{key: ch.Name, channel: ch.Name, stream: store.ChannelStream(me.ws.Name),
			subject: store.ChannelSubject(me.ws.Name, ch.Name)}}, false, nil
	}
	if scope != scopeDirect {
		if srcs, err = s.readable(ctx, me); err != nil {
			return nil, false, err
		}
	}
	if scope == "" || scope == scopeAll || scope == scopeDirect {
		srcs = append([]*source{directSource(me)}, srcs...)
	}
	for i, src := range srcs {
		src.order = i
	}
	return srcs, scope == scopeMentions, nil
}

// decode reads an item from a stored entry, or nil if it isn't one
// (approval answers, notices, digests and pushed copies in the inbox).
func decode(src *source, me *party, e *store.Entry) *item {
	if src.channel == "" {
		var d messagingv1.Delivery
		if proto.Unmarshal(e.Data, &d) != nil || d.GetMessage() == nil {
			return nil
		}
		m := d.GetMessage()
		return &item{src: src, seq: e.Seq, at: e.Time, it: &messagingv1.InboxItem{Id: m.Id, From: m.From,
			FromKind: m.FromKind, Body: m.Body, SentAt: m.SentAt}}
	}
	var p messagingv1.ChannelPost
	if proto.Unmarshal(e.Data, &p) != nil {
		return nil
	}
	return &item{src: src, seq: e.Seq, at: e.Time, mine: p.From == me.addr, it: &messagingv1.InboxItem{Id: p.Id,
		Channel: p.Channel, From: p.From, FromKind: p.FromKind, Body: p.Body, SentAt: p.SentAt,
		MentionsMe: slices.Contains(p.Mentions, me.addr)}}
}

// scan reads src from sequence from, keeping the items keep accepts; keep
// gets whether the item is already read.
func (s *Server) scan(ctx context.Context, me *party, src *source, from uint64, keep func(it *item, read bool) bool) (*scanned, error) {
	rs, err := s.st.ReadState(ctx, me.ws.Name, me.p.Name, src.key)
	if err != nil {
		return nil, unavailable(err)
	}
	if from == 0 {
		from = rs.Upto + 1
	}
	sc := &scanned{src: src, rs: rs, from: from, items: map[uint64]bool{}}
	err = s.st.Scan(ctx, src.stream, []string{src.subject}, from, func(e *store.Entry) bool {
		sc.seqs = append(sc.seqs, e.Seq)
		it := decode(src, me, e)
		if it == nil {
			return true
		}
		if !it.mine {
			sc.items[e.Seq] = true
		}
		read := it.mine || rs.Read(e.Seq)
		it.it.Read = read
		if keep(it, read) {
			sc.list = append(sc.list, it)
		}
		return true
	})
	if err != nil {
		return nil, unavailable(err)
	}
	return sc, nil
}

// markRead marks seqs of a scanned source read, and moves the cursor over
// everything read or not an item.
func (s *Server) markRead(ctx context.Context, me *party, sc *scanned, seqs []uint64) error {
	err := s.st.UpdateReadState(ctx, me.ws.Name, me.p.Name, sc.src.key, func(r *store.ReadState) {
		r.Mark(seqs...)
		if sc.from <= r.Upto+1 {
			r.Advance(sc.seqs, func(q uint64) bool { return !sc.items[q] })
		}
	})
	if err != nil {
		return unavailable(err)
	}
	return nil
}

// cursor is the opaque read_messages cursor: per source, the next sequence
// to read (unread pages, forward) or the sequence to read below (history,
// backward).
type cursor struct {
	Unread bool              `json:"u"`
	Pos    map[string]uint64 `json:"p"`
}

func (c *cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("cursor is not one read_messages returned"))
	}
	return &c, nil
}

func byTime(a, b *item) int {
	return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.src.order, b.src.order), cmp.Compare(a.seq, b.seq))
}

// ReadMessages implements MessagingService.
func (s *Server) ReadMessages(ctx context.Context, req *connect.Request[messagingv1.ReadMessagesRequest]) (*connect.Response[messagingv1.ReadMessagesResponse], error) {
	me, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	m := req.Msg
	unread := m.UnreadOnly == nil || *m.UnreadOnly
	mark := m.MarkRead == nil || *m.MarkRead
	limit := int(m.Limit)
	if limit <= 0 {
		limit = defaultPage
	}
	limit = min(limit, maxPage)
	srcs, mentionsOnly, err := s.scope(ctx, me, m.Scope)
	if err != nil {
		return nil, err
	}
	cur, err := decodeCursor(m.Cursor)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.Unread != unread {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("this cursor is for a different unread_only setting"))
	}
	pos := func(key string, def uint64) uint64 {
		if cur != nil {
			if p, ok := cur.Pos[key]; ok {
				return p
			}
		}
		return def
	}
	match := func(it *item) bool { return !mentionsOnly || it.it.MentionsMe }

	var all []*item
	var scans []*scanned
	unreadTotal, matching := 0, 0
	for _, src := range srcs {
		var sc *scanned
		if unread {
			// From the read cursor, so unread items before a page cursor
			// still count as remaining.
			start := pos(src.key, 0)
			sc, err = s.scan(ctx, me, src, 0, func(it *item, read bool) bool {
				if read || !match(it) {
					return false
				}
				unreadTotal++
				return it.seq >= start
			})
			if err == nil {
				matching += len(sc.list)
				all = append(all, sc.list...)
			}
		} else {
			before := pos(src.key, ^uint64(0))
			sc, err = s.scan(ctx, me, src, 1, func(it *item, read bool) bool {
				if !read && match(it) {
					unreadTotal++
				}
				return it.seq < before && match(it)
			})
			if err == nil {
				matching += len(sc.list)
				all = append(all, sc.list[max(0, len(sc.list)-limit):]...)
			}
		}
		if err != nil {
			return nil, err
		}
		scans = append(scans, sc)
	}
	slices.SortFunc(all, byTime)
	page := all
	if unread {
		page = all[:min(limit, len(all))]
	} else {
		page = all[max(0, len(all)-limit):]
	}

	out := &messagingv1.ReadMessagesResponse{}
	taken := map[*source][]uint64{}
	for _, it := range page {
		if !it.it.Read {
			taken[it.src] = append(taken[it.src], it.seq)
			unreadTotal--
		}
		item := proto.Clone(it.it).(*messagingv1.InboxItem)
		if r := []rune(item.Body); !m.Full && len(r) > previewRunes {
			item.Body, item.Truncated = string(r[:previewRunes]), true
		}
		out.Items = append(out.Items, item)
	}
	out.UnreadRemaining = int32(unreadTotal)
	if matching > len(page) {
		next := &cursor{Unread: unread, Pos: map[string]uint64{}}
		for _, sc := range scans {
			var mine []*item
			for _, it := range page {
				if it.src == sc.src {
					mine = append(mine, it)
				}
			}
			switch {
			case unread && len(mine) > 0:
				next.Pos[sc.src.key] = mine[len(mine)-1].seq + 1
			case unread:
				next.Pos[sc.src.key] = max(sc.from, pos(sc.src.key, 0))
			case len(mine) > 0:
				next.Pos[sc.src.key] = mine[0].seq
			default:
				if p := pos(sc.src.key, 0); p != 0 {
					next.Pos[sc.src.key] = p
				}
			}
		}
		out.NextCursor = next.encode()
	}
	if mark {
		for _, sc := range scans {
			if len(taken[sc.src]) > 0 {
				if err := s.markRead(ctx, me, sc, taken[sc.src]); err != nil {
					return nil, err
				}
			}
		}
	}
	return connect.NewResponse(out), nil
}

// find looks an item up by id in srcs.
func (s *Server) find(ctx context.Context, me *party, srcs []*source, id string) (*item, error) {
	since := time.Time{}
	if u, err := ulid.ParseStrict(id); err == nil {
		// Allow for the sender's clock being ahead.
		since = ulid.Time(u.Time()).Add(-time.Hour)
	}
	for _, src := range srcs {
		var found *item
		fn := func(e *store.Entry) bool {
			if it := decode(src, me, e); it != nil && it.it.Id == id {
				found = it
				return false
			}
			return true
		}
		var err error
		if !since.IsZero() {
			err = s.st.ScanSince(ctx, src.stream, []string{src.subject}, since, fn)
		}
		if err == nil && found == nil {
			err = s.st.Scan(ctx, src.stream, []string{src.subject}, 1, fn)
		}
		if err != nil {
			return nil, unavailable(err)
		}
		if found != nil {
			rs, err := s.st.ReadState(ctx, me.ws.Name, me.p.Name, src.key)
			if err != nil {
				return nil, unavailable(err)
			}
			found.it.Read = found.mine || rs.Read(found.seq)
			return found, nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no message %s among your direct messages and the channels you read", id))
}

// GetMessage implements MessagingService.
func (s *Server) GetMessage(ctx context.Context, req *connect.Request[messagingv1.GetMessageRequest]) (*connect.Response[messagingv1.GetMessageResponse], error) {
	me, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	srcs, _, err := s.scope(ctx, me, scopeAll)
	if err != nil {
		return nil, err
	}
	it, err := s.find(ctx, me, srcs, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&messagingv1.GetMessageResponse{Item: it.it}), nil
}

// MarkRead implements MessagingService.
func (s *Server) MarkRead(ctx context.Context, req *connect.Request[messagingv1.MarkReadRequest]) (*connect.Response[messagingv1.MarkReadResponse], error) {
	me, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	srcs, mentionsOnly, err := s.scope(ctx, me, req.Msg.Scope)
	if err != nil {
		return nil, err
	}
	var upTo *item
	if req.Msg.UpTo != "" {
		if upTo, err = s.find(ctx, me, srcs, req.Msg.UpTo); err != nil {
			return nil, err
		}
	}
	for _, src := range srcs {
		sc, err := s.scan(ctx, me, src, 0, func(it *item, read bool) bool {
			if read || (mentionsOnly && !it.it.MentionsMe) {
				return false
			}
			if upTo == nil {
				return true
			}
			if it.src == upTo.src {
				return it.seq <= upTo.seq
			}
			return !it.at.After(upTo.at)
		})
		if err != nil {
			return nil, err
		}
		var seqs []uint64
		for _, it := range sc.list {
			seqs = append(seqs, it.seq)
		}
		if err := s.markRead(ctx, me, sc, seqs); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&messagingv1.MarkReadResponse{}), nil
}

// unreadCounts counts unread items by scope: direct, mentions and each
// readable channel.
func (s *Server) unreadCounts(ctx context.Context, me *party) (map[string]int, error) {
	srcs, _, err := s.scope(ctx, me, scopeAll)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{scopeDirect: 0, scopeMentions: 0}
	for _, src := range srcs {
		sc, err := s.scan(ctx, me, src, 0, func(it *item, read bool) bool { return !read })
		if err != nil {
			return nil, err
		}
		counts[src.key] = len(sc.list)
		for _, it := range sc.list {
			if it.it.MentionsMe {
				counts[scopeMentions]++
			}
		}
	}
	return counts, nil
}

// UnreadSummary implements MessagingService.
func (s *Server) UnreadSummary(ctx context.Context, req *connect.Request[messagingv1.UnreadSummaryRequest]) (*connect.Response[messagingv1.UnreadSummaryResponse], error) {
	me, err := s.sender(ctx, "")
	if err != nil {
		return nil, err
	}
	counts, err := s.unreadCounts(ctx, me)
	if err != nil {
		return nil, err
	}
	out := &messagingv1.UnreadSummaryResponse{}
	add := func(scope string) {
		out.Counts = append(out.Counts, &messagingv1.UnreadCount{Scope: scope, Unread: int32(counts[scope])})
	}
	add(scopeDirect)
	add(scopeMentions)
	for _, ch := range me.ws.Messaging.Channels {
		if _, ok := counts[ch.Name]; ok {
			add(ch.Name)
		}
	}
	return connect.NewResponse(out), nil
}

// markPushedRead marks an item the member acked as read: a pushed direct
// message or post counts as read (design §7.8). Best effort.
func (s *Server) markPushedRead(ctx context.Context, me *party, deliveryID string) {
	seq := store.DeliverySeq(deliveryID)
	if seq == 0 {
		return
	}
	e, err := s.st.Get(ctx, store.InboxStream(me.ws.Name), seq)
	if err != nil || e == nil {
		return
	}
	var d messagingv1.Delivery
	if proto.Unmarshal(e.Data, &d) != nil {
		return
	}
	key := scopeDirect
	switch {
	case d.GetMessage() != nil:
	case d.GetPost() != nil && e.ChanSeq() > 0:
		key, seq = d.GetPost().Channel, e.ChanSeq()
	default:
		return
	}
	var compact bool
	err = s.st.UpdateReadState(ctx, me.ws.Name, me.p.Name, key, func(r *store.ReadState) {
		r.Mark(seq)
		compact = len(r.Extra) > store.MaxExtra/4
	})
	if err != nil {
		s.log.Warn("mark pushed item read", "member", me.addr, "err", err)
		return
	}
	if compact {
		// Fold the read items into the cursor, so Extra stays small.
		src := directSource(me)
		if key != scopeDirect {
			src = &source{key: key, channel: key, stream: store.ChannelStream(me.ws.Name), subject: store.ChannelSubject(me.ws.Name, key)}
		}
		if sc, err := s.scan(ctx, me, src, 0, func(*item, bool) bool { return false }); err == nil {
			_ = s.markRead(ctx, me, sc, nil)
		}
	}
}

