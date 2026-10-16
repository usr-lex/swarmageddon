package store

import (
	"context"
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
	AuditVerdict  = "verdict"
	AuditPost     = "post"
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
	subjects := []string{"audit." + q.Workspace + ".>"}
	if len(q.Kinds) > 0 {
		subjects = nil
		for _, k := range q.Kinds {
			subjects = append(subjects, "audit."+q.Workspace+"."+k)
		}
	}
	until := q.Until
	if until.IsZero() {
		until = time.Now()
	}
	sent := 0
	var emitErr error
	fn := func(e *Entry) bool {
		var rec messagingv1.AuditRecord
		if proto.Unmarshal(e.Data, &rec) != nil {
			return true
		}
		if rec.GetAt().AsTime().After(until) {
			return false
		}
		if q.Address != "" && rec.From != q.Address && rec.To != q.Address {
			return true
		}
		if emitErr = emit(&rec); emitErr != nil {
			return false
		}
		sent++
		return q.Limit <= 0 || sent < q.Limit
	}
	cfg := jetstream.ConsumerConfig{DeliverPolicy: jetstream.DeliverAllPolicy}
	if !q.Since.IsZero() {
		cfg = jetstream.ConsumerConfig{DeliverPolicy: jetstream.DeliverByStartTimePolicy, OptStartTime: &q.Since}
	}
	err := s.scan(ctx, AuditStream(q.Workspace), subjects, cfg, ^uint64(0), fn)
	if emitErr != nil {
		return emitErr
	}
	return err
}
