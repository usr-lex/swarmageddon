package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// MaxScan bounds one scan, so a huge backlog can't stall a call.
const MaxScan = 50000

// Scan calls fn with the messages on subjects in a stream, in order,
// starting at sequence start, until fn returns false, MaxScan messages were
// read, or the end.
func (s *Store) Scan(ctx context.Context, stream string, subjects []string, start uint64, fn func(*Entry) bool) error {
	return s.scan(ctx, stream, subjects, jetstream.ConsumerConfig{
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: max(start, 1)}, MaxScan, fn)
}

// ScanSince is Scan from the first message stored at or after since.
func (s *Store) ScanSince(ctx context.Context, stream string, subjects []string, since time.Time, fn func(*Entry) bool) error {
	return s.scan(ctx, stream, subjects, jetstream.ConsumerConfig{
		DeliverPolicy: jetstream.DeliverByStartTimePolicy, OptStartTime: &since}, MaxScan, fn)
}

func (s *Store) scan(ctx context.Context, stream string, subjects []string, cfg jetstream.ConsumerConfig, limit uint64, fn func(*Entry) bool) error {
	if len(subjects) == 0 {
		return nil
	}
	st, err := s.js.Stream(ctx, stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cfg.AckPolicy = jetstream.AckNonePolicy
	cfg.FilterSubjects = subjects
	cfg.InactiveThreshold = time.Minute
	cfg.MemoryStorage = true
	cfg.Replicas = 1
	cons, err := st.CreateConsumer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("scan %s: %w", stream, err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = st.DeleteConsumer(dctx, cons.CachedInfo().Name)
	}()
	remaining := min(cons.CachedInfo().NumPending, limit)
	retries := 0
	for remaining > 0 {
		batch, err := cons.Fetch(int(min(remaining, 256)), jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			return fmt.Errorf("scan %s: fetch: %w", stream, err)
		}
		got := 0
		for m := range batch.Messages() {
			got++
			remaining--
			meta, err := m.Metadata()
			if err != nil {
				continue
			}
			if !fn(&Entry{Seq: meta.Sequence.Stream, Subject: m.Subject(), Header: m.Headers(), Data: m.Data(), Time: meta.Timestamp}) {
				return nil
			}
		}
		if err := batch.Error(); got == 0 && errors.Is(err, nats.ErrNoResponders) && retries < 40 {
			// A new consumer may not be ready for pull requests yet.
			retries++
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
			return fmt.Errorf("scan %s: batch: %w", stream, err)
		}
		if got == 0 {
			break
		}
	}
	return nil
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

// get reads a key from the stream leader. KV handles use direct gets,
// which any replica may answer, possibly with a value older than a write
// this replica just made; read cursors must read their own writes.
func (s *Store) get(ctx context.Context, bucket, key string) (data []byte, rev uint64, err error) {
	req, _ := json.Marshal(map[string]string{"last_by_subj": "$KV." + bucket + "." + key})
	msg, err := s.nc.RequestWithContext(ctx, "$JS.API.STREAM.MSG.GET.KV_"+bucket, req)
	if err != nil {
		return nil, 0, fmt.Errorf("get %s/%s: %w", bucket, key, err)
	}
	var resp struct {
		Message *struct {
			Seq  uint64 `json:"seq"`
			Hdrs []byte `json:"hdrs"`
			Data []byte `json:"data"`
		} `json:"message"`
		Error *struct {
			Code        int    `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, 0, err
	}
	switch {
	case resp.Error != nil && resp.Error.Code == 404:
		return nil, 0, jetstream.ErrKeyNotFound
	case resp.Error != nil:
		return nil, 0, fmt.Errorf("get %s/%s: %s", bucket, key, resp.Error.Description)
	case resp.Message == nil:
		return nil, 0, jetstream.ErrKeyNotFound
	case bytes.Contains(resp.Message.Hdrs, []byte("KV-Operation")):
		// A delete or purge marker.
		return nil, resp.Message.Seq, jetstream.ErrKeyNotFound
	}
	return resp.Message.Data, resp.Message.Seq, nil
}

// ReadState returns a member's read state in a scope.
func (s *Store) ReadState(ctx context.Context, ws, member, scope string) (ReadState, error) {
	var r ReadState
	data, _, err := s.get(ctx, BucketCursors, cursorKey(ws, member, scope))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(data, &r)
}

// UpdateReadState applies f to a member's read state, retrying on conflicts.
func (s *Store) UpdateReadState(ctx context.Context, ws, member, scope string, f func(*ReadState)) error {
	key := cursorKey(ws, member, scope)
	for range 20 {
		var r ReadState
		data, rev, err := s.get(ctx, BucketCursors, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
		case err != nil:
			return err
		default:
			if err := json.Unmarshal(data, &r); err != nil {
				return err
			}
		}
		before, extra := r.Upto, slices.Clone(r.Extra)
		f(&r)
		if r.Upto == before && slices.Equal(r.Extra, extra) {
			return nil
		}
		data, _ = json.Marshal(r)
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
	data, _, err := s.get(ctx, BucketDelivery, cursorKey(ws, member, channel))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
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
	data, _, err := s.get(ctx, BucketDigests, cursorKey(ws, member, channel))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(data, &d)
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
	data, _, err := s.get(ctx, BucketDupes, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return "", nil // expired in between
		}
		return "", err
	}
	if prev := string(data); prev != id {
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
