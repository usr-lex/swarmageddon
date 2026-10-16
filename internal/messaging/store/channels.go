package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// Channels storage (design §7.8).
const (
	BucketCursors  = "cursors"  // key <ws>.<member>.<scope>: ReadState
	BucketDelivery = "delivery" // key <ws>.<member>.<channel>: push, digest or pull
	BucketDigests  = "digests"  // key <ws>.<member>.<channel>: DigestState
	BucketDupes    = "dupes"    // key <ws>.<sender>.<hash of target and body>: message id; TTL DedupWindow

	// HeaderChanSeq on an inbox copy of a post is the post's sequence in
	// the CHANNELS stream.
	HeaderChanSeq = "Swarm-Chan-Seq"
)

// ChannelStream is the posts stream of a workspace; subjects chan.<ws>.<channel>.
func ChannelStream(ws string) string { return "CHANNELS_" + ws }

// ChannelSubject is the subject of one channel's posts.
func ChannelSubject(ws, channel string) string { return "chan." + ws + "." + channel }

// PublishPost stores a post; seq is its sequence in the channel stream (the
// original's, for a duplicate).
func (s *Store) PublishPost(ctx context.Context, ws string, p *messagingv1.ChannelPost) (seq uint64, dup bool, err error) {
	data, err := proto.Marshal(p)
	if err != nil {
		return 0, false, err
	}
	ack, err := s.js.PublishMsg(ctx, &nats.Msg{Subject: ChannelSubject(ws, p.Channel), Data: data}, jetstream.WithMsgID(p.Id))
	if err != nil {
		return 0, false, err
	}
	return ack.Sequence, ack.Duplicate, nil
}

// DeliverPost stores a pushed copy of a post (or a mention) in a recipient's
// inbox, carrying the post's channel sequence.
func (s *Store) DeliverPost(ctx context.Context, ws, recipient, msgID string, d *messagingv1.Delivery, chanSeq uint64) (bool, error) {
	data, err := proto.Marshal(d)
	if err != nil {
		return false, err
	}
	m := &nats.Msg{Subject: InboxSubject(ws, recipient), Data: data, Header: nats.Header{}}
	m.Header.Set(HeaderChanSeq, strconv.FormatUint(chanSeq, 10))
	ack, err := s.js.PublishMsg(ctx, m, jetstream.WithMsgID(msgID))
	if err != nil {
		return false, err
	}
	return ack.Duplicate, nil
}

// Entry is one stored message.
type Entry struct {
	Seq     uint64
	Subject string
	Header  nats.Header
	Data    []byte
	Time    time.Time
}

// ChanSeq is the channel sequence an inbox copy of a post carries, or 0.
func (e *Entry) ChanSeq() uint64 {
	n, _ := strconv.ParseUint(e.Header.Get(HeaderChanSeq), 10, 64)
	return n
}

// Scan calls fn with the messages on subjects in a stream, in order,
// starting at sequence start, until fn returns false, limit messages were
// read (at most limit), or the end. It returns the last sequence read (start-1 if none) and
// whether it stopped at limit.
func (s *Store) Scan(ctx context.Context, stream string, subjects []string, start uint64, limit int, fn func(*Entry) bool) (last uint64, capped bool, err error) {
	last = start - 1
	if len(subjects) == 0 {
		return last, false, nil
	}
	st, err := s.js.Stream(ctx, stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return last, false, nil
	}
	if err != nil {
		return last, false, err
	}
	cfg := jetstream.ConsumerConfig{
		AckPolicy:         jetstream.AckNonePolicy,
		DeliverPolicy:     jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:       max(start, 1),
		FilterSubjects:    subjects,
		InactiveThreshold: time.Minute,
		MemoryStorage:     true,
		Replicas:          1,
	}
	cons, err := st.CreateConsumer(ctx, cfg)
	if err != nil {
		return last, false, err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = st.DeleteConsumer(dctx, cons.CachedInfo().Name)
	}()
	remaining := cons.CachedInfo().NumPending
	read := 0
	for remaining > 0 {
		if read >= limit {
			return last, true, nil
		}
		batch, err := cons.Fetch(int(min(remaining, 256, uint64(limit-read))), jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			return last, false, err
		}
		got := 0
		for m := range batch.Messages() {
			got++
			remaining--
			read++
			meta, err := m.Metadata()
			if err != nil {
				continue
			}
			last = meta.Sequence.Stream
			if !fn(&Entry{Seq: last, Subject: m.Subject(), Header: m.Headers(), Data: m.Data(), Time: meta.Timestamp}) {
				return last, false, nil
			}
		}
		if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
			return last, false, err
		}
		if got == 0 {
			break
		}
	}
	return last, false, nil
}

// LastSeq is the stream's last sequence, or 0.
func (s *Store) LastSeq(ctx context.Context, stream string) (uint64, error) {
	st, err := s.js.Stream(ctx, stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	info, err := st.Info(ctx)
	if err != nil {
		return 0, err
	}
	return info.State.LastSeq, nil
}

// LastSeqOf is the sequence of the last message on subject, or 0.
func (s *Store) LastSeqOf(ctx context.Context, stream, subject string) (uint64, error) {
	st, err := s.js.Stream(ctx, stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	m, err := st.GetLastMsgForSubject(ctx, subject)
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return m.Sequence, nil
}

// Get reads one message by sequence; nil if it is gone.
func (s *Store) Get(ctx context.Context, stream string, seq uint64) (*Entry, error) {
	st, err := s.js.Stream(ctx, stream)
	if err != nil {
		return nil, err
	}
	m, err := st.GetMsg(ctx, seq)
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Entry{Seq: m.Sequence, Subject: m.Subject, Header: m.Header, Data: m.Data, Time: m.Time}, nil
}

// ReadState is a member's read position in one scope (direct messages or a
// channel): everything up to Upto is read, plus the sequences in Extra.
type ReadState struct {
	Upto  uint64   `json:"upto"`
	Extra []uint64 `json:"extra,omitempty"`
}

// MaxExtra bounds ReadState.Extra; the oldest entries go first.
const MaxExtra = 1024

// Read reports whether seq is read.
func (r *ReadState) Read(seq uint64) bool {
	return seq <= r.Upto || slices.Contains(r.Extra, seq)
}

// Mark marks seqs read.
func (r *ReadState) Mark(seqs ...uint64) {
	for _, q := range seqs {
		if !r.Read(q) {
			r.Extra = append(r.Extra, q)
		}
	}
	slices.Sort(r.Extra)
	if n := len(r.Extra); n > MaxExtra {
		r.Extra = r.Extra[n-MaxExtra:]
	}
}

// Advance moves Upto along seqs, the scope's sequences in order from a
// point at or before Upto+1, while they are read or not items (skip).
func (r *ReadState) Advance(seqs []uint64, skip func(uint64) bool) {
	for _, q := range seqs {
		if q <= r.Upto {
			continue
		}
		if !r.Read(q) && !skip(q) {
			break
		}
		r.Upto = q
	}
	r.Extra = slices.DeleteFunc(r.Extra, func(q uint64) bool { return q <= r.Upto })
}

func cursorKey(ws, member, scope string) string { return ws + "." + member + "." + scope }

// ReadState returns a member's read state in a scope.
func (s *Store) ReadState(ctx context.Context, ws, member, scope string) (ReadState, error) {
	var r ReadState
	e, err := s.cursors.Get(ctx, cursorKey(ws, member, scope))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(e.Value(), &r)
}

// UpdateReadState applies f to a member's read state, retrying on conflicts.
func (s *Store) UpdateReadState(ctx context.Context, ws, member, scope string, f func(*ReadState)) error {
	key := cursorKey(ws, member, scope)
	for range 20 {
		var r ReadState
		var rev uint64
		e, err := s.cursors.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
		case err != nil:
			return err
		default:
			rev = e.Revision()
			if err := json.Unmarshal(e.Value(), &r); err != nil {
				return err
			}
		}
		before, extra := r.Upto, slices.Clone(r.Extra)
		f(&r)
		if r.Upto == before && slices.Equal(r.Extra, extra) {
			return nil
		}
		data, _ := json.Marshal(r)
		if rev == 0 {
			_, err = s.cursors.Create(ctx, key, data)
		} else {
			_, err = s.cursors.Update(ctx, key, data, rev)
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) && !isWrongSequence(err) {
			return err
		}
	}
	return errors.New("read cursor keeps changing")
}

func isWrongSequence(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && (apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence ||
		apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequenceConstant)
}

// Delivery returns a member's delivery mode for a channel, or "" if unset.
func (s *Store) Delivery(ctx context.Context, ws, member, channel string) (string, error) {
	e, err := s.delivery.Get(ctx, cursorKey(ws, member, channel))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(e.Value()), nil
}

// SetDelivery sets a member's delivery mode for a channel.
func (s *Store) SetDelivery(ctx context.Context, ws, member, channel, mode string) error {
	_, err := s.delivery.Put(ctx, cursorKey(ws, member, channel), []byte(mode))
	return err
}

// DigestState is where a member's digests of a channel stand.
type DigestState struct {
	// Seq is the last post covered by a digest.
	Seq uint64 `json:"seq"`
	// Sent is when the last digest went out.
	Sent time.Time `json:"sent"`
}

// Digest returns a member's digest state for a channel.
func (s *Store) Digest(ctx context.Context, ws, member, channel string) (DigestState, error) {
	var d DigestState
	e, err := s.digests.Get(ctx, cursorKey(ws, member, channel))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(e.Value(), &d)
}

// SetDigest records a member's digest state for a channel.
func (s *Store) SetDigest(ctx context.Context, ws, member, channel string, d DigestState) error {
	data, _ := json.Marshal(d)
	_, err := s.digests.Put(ctx, cursorKey(ws, member, channel), data)
	return err
}

// SameBody records that sender sent body to target as message id, and
// returns the id of an earlier, different message with the same body to the
// same target within DedupWindow, or "".
func (s *Store) SameBody(ctx context.Context, ws, sender, target, body, id string) (string, error) {
	h := sha256.Sum256([]byte(target + "\x00" + body))
	key := ws + "." + sender + "." + hex.EncodeToString(h[:16])
	if _, err := s.dupes.Create(ctx, key, []byte(id)); err == nil {
		return "", nil
	} else if !errors.Is(err, jetstream.ErrKeyExists) {
		return "", err
	}
	e, err := s.dupes.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return "", nil // expired in between
		}
		return "", err
	}
	if prev := string(e.Value()); prev != id {
		return prev, nil
	}
	return "", nil
}

// purgeKeys deletes a workspace's keys from the channel buckets.
func (s *Store) purgeKeys(ctx context.Context, ws string) error {
	for _, kv := range []jetstream.KeyValue{s.cursors, s.delivery, s.digests} {
		lister, err := kv.ListKeysFiltered(ctx, ws+".>")
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			continue
		}
		if err != nil {
			return err
		}
		for k := range lister.Keys() {
			if err := kv.Purge(ctx, k); err != nil {
				return err
			}
		}
	}
	return nil
}
