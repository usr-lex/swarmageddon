package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"
)

// Layout (ADR 0002). One stream per workspace and kind, so each workspace's
// retention is simply that stream's MaxAge.
const (
	BucketPresence = "presence" // key <ws>.<name>, TTL 90 s
	BucketStatus   = "status"   // key <ws>.<name>, latest RoomStatusReport
	BucketPeople   = "people"   // key <ws>.<handle>, people the control center acted for

	PresenceTTL = 90 * time.Second
	// DedupWindow is the inbox's duplicate window for Nats-Msg-Id.
	DedupWindow = 10 * time.Minute
)

// InboxStream is the inbox stream of a workspace; subjects inbox.<ws>.<name>.
func InboxStream(ws string) string { return "INBOX_" + ws }

// AuditStream is the audit stream of a workspace; subjects audit.<ws>.<kind>.
func AuditStream(ws string) string { return "AUDIT_" + ws }

// ApprovalStream is the approvals stream of a workspace; subjects approval.<ws>.<room>.
func ApprovalStream(ws string) string { return "APPROVALS_" + ws }

// InboxSubject is the subject of one recipient's inbox.
func InboxSubject(ws, name string) string { return "inbox." + ws + "." + name }

// WorkspaceConfig is the per-workspace stream settings; 0 keeps forever.
type WorkspaceConfig struct {
	MessageRetention  time.Duration
	AuditRetention    time.Duration
	ApprovalRetention time.Duration
}

// Store is the messaging service's storage on one replica.
type Store struct {
	node *Node
	js   jetstream.JetStream
	nc   *nats.Conn
	log  *slog.Logger

	// AckWait is how long a delivery waits for its ack before redelivery.
	AckWait time.Duration

	presence, status, people jetstream.KeyValue

	mu      sync.Mutex
	applied map[string]WorkspaceConfig
	known   map[string]bool // people known to exist, "<ws>.<handle>"
}

// Open waits for JetStream and creates the KV buckets.
func Open(ctx context.Context, node *Node, log *slog.Logger) (*Store, error) {
	if err := node.WaitJetStream(ctx); err != nil {
		return nil, err
	}
	s := &Store{node: node, js: node.js, nc: node.nc, log: log, AckWait: 2 * time.Minute,
		applied: map[string]WorkspaceConfig{}, known: map[string]bool{}}
	buckets := []struct {
		kv  *jetstream.KeyValue
		cfg jetstream.KeyValueConfig
	}{
		{&s.presence, jetstream.KeyValueConfig{Bucket: BucketPresence, TTL: PresenceTTL, LimitMarkerTTL: 10 * time.Minute}},
		{&s.status, jetstream.KeyValueConfig{Bucket: BucketStatus}},
		{&s.people, jetstream.KeyValueConfig{Bucket: BucketPeople}},
	}
	for _, b := range buckets {
		b.cfg.Replicas = node.replicas
		b.cfg.Storage = jetstream.FileStorage
		kv, err := retry(ctx, func(ctx context.Context) (jetstream.KeyValue, error) {
			return s.js.CreateOrUpdateKeyValue(ctx, b.cfg)
		})
		if err != nil {
			return nil, fmt.Errorf("kv %s: %w", b.cfg.Bucket, err)
		}
		*b.kv = kv
	}
	return s, nil
}

// retry calls f until it succeeds or ctx ends; JetStream may briefly lack a
// leader while a cluster forms or a replica restarts.
func retry[T any](ctx context.Context, f func(context.Context) (T, error)) (T, error) {
	for {
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		v, err := f(actx)
		cancel()
		if err == nil {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// EnsureWorkspace creates or updates a workspace's streams. It is cheap when
// nothing changed since the last call on this replica.
func (s *Store) EnsureWorkspace(ctx context.Context, ws string, cfg WorkspaceConfig) error {
	s.mu.Lock()
	prev, ok := s.applied[ws]
	s.mu.Unlock()
	if ok && prev == cfg {
		return nil
	}
	streams := []jetstream.StreamConfig{
		{Name: InboxStream(ws), Subjects: []string{"inbox." + ws + ".*"}, MaxAge: cfg.MessageRetention,
			Duplicates: dedup(DedupWindow, cfg.MessageRetention)},
		{Name: AuditStream(ws), Subjects: []string{"audit." + ws + ".*"}, MaxAge: cfg.AuditRetention,
			Duplicates: dedup(2*time.Minute, cfg.AuditRetention)},
		{Name: ApprovalStream(ws), Subjects: []string{"approval." + ws + ".*"}, MaxAge: cfg.ApprovalRetention,
			Duplicates: dedup(DedupWindow, cfg.ApprovalRetention)},
	}
	for _, sc := range streams {
		sc.Storage = jetstream.FileStorage
		sc.Replicas = s.node.replicas
		sc.Retention = jetstream.LimitsPolicy
		sc.Discard = jetstream.DiscardOld
		if _, err := s.js.CreateOrUpdateStream(ctx, sc); err != nil {
			return fmt.Errorf("stream %s: %w", sc.Name, err)
		}
	}
	s.mu.Lock()
	s.applied[ws] = cfg
	s.mu.Unlock()
	return nil
}

func dedup(want, maxAge time.Duration) time.Duration {
	if maxAge > 0 && maxAge < want {
		return maxAge
	}
	return want
}

// publish stores a proto on subject with an optional dedup id.
func (s *Store) publish(ctx context.Context, subject, msgID string, m proto.Message) (bool, error) {
	data, err := proto.Marshal(m)
	if err != nil {
		return false, err
	}
	var opts []jetstream.PublishOpt
	if msgID != "" {
		opts = append(opts, jetstream.WithMsgID(msgID))
	}
	ack, err := s.js.PublishMsg(ctx, &nats.Msg{Subject: subject, Data: data}, opts...)
	if err != nil {
		return false, err
	}
	return ack.Duplicate, nil
}

// IsLeader reports whether this replica leads the stream (or runs alone).
func (s *Store) isLeader(ctx context.Context, stream string) bool {
	st, err := s.js.Stream(ctx, stream)
	if err != nil {
		return false
	}
	info, err := st.Info(ctx)
	if err != nil {
		return false
	}
	return info.Cluster == nil || info.Cluster.Leader == "" || info.Cluster.Leader == s.node.name
}

// RegisterPerson records that a person exists in a workspace.
func (s *Store) RegisterPerson(ctx context.Context, ws, handle string) error {
	key := ws + "." + handle
	s.mu.Lock()
	known := s.known[key]
	s.mu.Unlock()
	if known {
		return nil
	}
	if _, err := s.people.Create(ctx, key, []byte("{}")); err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
		return err
	}
	s.mu.Lock()
	s.known[key] = true
	s.mu.Unlock()
	return nil
}

// IsPerson reports whether handle is a known person of the workspace.
func (s *Store) IsPerson(ctx context.Context, ws, handle string) bool {
	key := ws + "." + handle
	s.mu.Lock()
	known := s.known[key]
	s.mu.Unlock()
	if known {
		return true
	}
	if _, err := s.people.Get(ctx, key); err != nil {
		return false
	}
	s.mu.Lock()
	s.known[key] = true
	s.mu.Unlock()
	return true
}

// People lists the known people of a workspace.
func (s *Store) People(ctx context.Context, ws string) ([]string, error) {
	lister, err := s.people.ListKeysFiltered(ctx, ws+".*")
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for k := range lister.Keys() {
		out = append(out, k[len(ws)+1:])
	}
	return out, nil
}
