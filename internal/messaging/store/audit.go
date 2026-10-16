package store

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// Audit kinds (subjects audit.<ws>.<kind>).
const (
	AuditMessage  = "message"
	AuditCrossing = "crossing"
	AuditRefusal  = "refusal"
	AuditPresence = "presence"
)

// Audit appends a record to audit.<ws>.<rec.Kind>.
func (s *Store) Audit(ctx context.Context, ws string, rec *messagingv1.AuditRecord) error {
	if rec.At == nil {
		rec.At = now()
	}
	_, err := s.publish(ctx, "audit."+ws+"."+rec.Kind, "", rec)
	return err
}

// AuditQuery selects audit records.
type AuditQuery struct {
	Workspace string
	Kinds     []string // empty: all
	Since     time.Time
	Until     time.Time // zero: now
	Address   string    // only records from or to this address
	Limit     int       // 0: no limit
}

// QueryAudit streams matching records, oldest first, to emit.
func (s *Store) QueryAudit(ctx context.Context, q AuditQuery, emit func(*messagingv1.AuditRecord) error) error {
	stream, err := s.js.Stream(ctx, AuditStream(q.Workspace))
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	cfg := jetstream.ConsumerConfig{
		AckPolicy:         jetstream.AckNonePolicy,
		DeliverPolicy:     jetstream.DeliverAllPolicy,
		InactiveThreshold: time.Minute,
		MemoryStorage:     true,
		Replicas:          1,
	}
	if !q.Since.IsZero() {
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		since := q.Since
		cfg.OptStartTime = &since
	}
	for _, k := range q.Kinds {
		cfg.FilterSubjects = append(cfg.FilterSubjects, "audit."+q.Workspace+"."+k)
	}
	cons, err := stream.CreateConsumer(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = stream.DeleteConsumer(dctx, cons.CachedInfo().Name)
	}()
	info, err := cons.Info(ctx)
	if err != nil {
		return err
	}
	remaining := info.NumPending
	until := q.Until
	if until.IsZero() {
		until = time.Now()
	}
	sent := 0
	for remaining > 0 {
		batch, err := cons.Fetch(int(min(remaining, 256)), jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			return err
		}
		got := 0
		for m := range batch.Messages() {
			got++
			remaining--
			var rec messagingv1.AuditRecord
			if proto.Unmarshal(m.Data(), &rec) != nil {
				continue
			}
			if rec.GetAt().AsTime().After(until) {
				return nil
			}
			if q.Address != "" && rec.From != q.Address && rec.To != q.Address {
				continue
			}
			if err := emit(&rec); err != nil {
				return err
			}
			if sent++; q.Limit > 0 && sent >= q.Limit {
				return nil
			}
		}
		if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) {
			return err
		}
		if got == 0 {
			return nil
		}
	}
	return nil
}
