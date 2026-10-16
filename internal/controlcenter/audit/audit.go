// Package audit records every control center action (docs/design.md §8.1).
// Each event goes to the log; once PostgreSQL is up it also goes to the
// audit table. Events from before the database existed (the bootstrap
// phase) are kept in memory and copied in when it appears (ADR 0008).
package audit

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// maxPending bounds the bootstrap buffer.
const maxPending = 10000

// Auditor records audit events.
type Auditor struct {
	// DB returns the store, or nil before the database exists.
	DB  func() *store.Store
	Log *slog.Logger

	mu      sync.Mutex
	pending []store.AuditEvent
}

// New returns an auditor logging to log.
func New(log *slog.Logger, db func() *store.Store) *Auditor {
	return &Auditor{DB: db, Log: log}
}

// Record writes one event. It never fails the caller's action.
func (a *Auditor) Record(ctx context.Context, ev store.AuditEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.Result == "" {
		ev.Result = "ok"
	}
	a.Log.LogAttrs(ctx, slog.LevelInfo, "audit",
		slog.String("actor", ev.Actor), slog.String("action", ev.Action), slog.String("object", ev.Object),
		slog.String("workspace", ev.Workspace), slog.String("sourceIP", ev.SourceIP), slog.String("result", ev.Result),
		slog.String("detail", ev.Detail))
	if db := a.db(); db != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := db.InsertAudit(ctx, ev); err == nil {
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) >= maxPending {
		a.pending = a.pending[1:]
	}
	a.pending = append(a.pending, ev)
}

func (a *Auditor) db() *store.Store {
	if a.DB == nil {
		return nil
	}
	return a.DB()
}

// Flush copies buffered events into the database.
func (a *Auditor) Flush(ctx context.Context) error {
	db := a.db()
	if db == nil {
		return nil
	}
	a.mu.Lock()
	evs := a.pending
	a.pending = nil
	a.mu.Unlock()
	if err := db.InsertAudit(ctx, evs...); err != nil {
		a.mu.Lock()
		a.pending = append(evs, a.pending...)
		a.mu.Unlock()
		return err
	}
	return nil
}

// Pending returns the buffered events (bootstrap phase), oldest first.
func (a *Auditor) Pending() []store.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]store.AuditEvent(nil), a.pending...)
}

// RetentionLoop deletes events older than retention() every hour; 0 keeps them.
func (a *Auditor) RetentionLoop(ctx context.Context, retention func(context.Context) time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if db := a.db(); db != nil {
			if r := retention(ctx); r > 0 {
				if n, err := db.DeleteAuditBefore(ctx, time.Now().Add(-r)); err != nil {
					a.Log.Warn("audit retention", "err", err)
				} else if n > 0 {
					a.Log.Info("audit retention removed events", "count", n)
				}
			}
			_ = a.Flush(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
