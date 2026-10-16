package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Verdict is a warden verdict and the human decision on it.
type Verdict struct {
	ID, Workspace, Room, Layer string
	Severity                   int // 1 low .. 4 critical
	Rule, Reason, Evidence     string
	Action, Source             string
	At                         time.Time
	Decision, DecidedBy        string // decision: "", dismiss, confirm
	DecidedAt                  *time.Time
	Note                       string
}

const verdictCols = `id, workspace, room, layer, severity, rule, reason, evidence, action, source, at, decision, decided_by, decided_at, note`

func scanVerdict(r pgx.Row) (Verdict, error) {
	var v Verdict
	err := r.Scan(&v.ID, &v.Workspace, &v.Room, &v.Layer, &v.Severity, &v.Rule, &v.Reason, &v.Evidence, &v.Action, &v.Source,
		&v.At, &v.Decision, &v.DecidedBy, &v.DecidedAt, &v.Note)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// InsertVerdict stores a verdict once; it reports whether it was new.
func (s *Store) InsertVerdict(ctx context.Context, v Verdict) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO verdicts (id, workspace, room, layer, severity, rule, reason, evidence, action, source, at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (id) DO NOTHING`,
		v.ID, v.Workspace, v.Room, v.Layer, v.Severity, v.Rule, v.Reason, truncate(v.Evidence, 20000), v.Action, v.Source, v.At)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// VerdictFilter selects verdicts.
type VerdictFilter struct {
	Workspace     string
	Room          string
	UndecidedOnly bool
	MinSeverity   int
	Before        time.Time
	Limit         int
}

// ListVerdicts returns verdicts newest first.
func (s *Store) ListVerdicts(ctx context.Context, f VerdictFilter) ([]Verdict, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	if f.Before.IsZero() {
		f.Before = time.Now().Add(time.Hour)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+verdictCols+` FROM verdicts WHERE workspace=$1 AND ($2='' OR room=$2)
		AND (NOT $3 OR decision='') AND severity >= $4 AND at < $5 ORDER BY at DESC LIMIT $6`,
		f.Workspace, f.Room, f.UndecidedOnly, f.MinSeverity, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Verdict, error) { return scanVerdict(r) })
}

// GetVerdict returns one verdict.
func (s *Store) GetVerdict(ctx context.Context, id string) (Verdict, error) {
	return scanVerdict(s.pool.QueryRow(ctx, `SELECT `+verdictCols+` FROM verdicts WHERE id=$1`, id))
}

// DecideVerdict records a person's decision.
func (s *Store) DecideVerdict(ctx context.Context, id, decision, by, note string) (Verdict, error) {
	return scanVerdict(s.pool.QueryRow(ctx, `UPDATE verdicts SET decision=$2, decided_by=$3, decided_at=now(), note=$4 WHERE id=$1
		RETURNING `+verdictCols, id, decision, by, note))
}

// VerdictBadge is a room's undecided verdicts of the last day.
type VerdictBadge struct {
	MaxSeverity int
	Undecided   int
}

// VerdictBadges returns badges per workspace/room.
func (s *Store) VerdictBadges(ctx context.Context, since time.Time) (map[string]VerdictBadge, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace, room, max(severity), count(*) FROM verdicts
		WHERE decision='' AND at >= $1 GROUP BY 1,2`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]VerdictBadge{}
	for rows.Next() {
		var ws, room string
		var b VerdictBadge
		if err := rows.Scan(&ws, &room, &b.MaxSeverity, &b.Undecided); err != nil {
			return nil, err
		}
		out[ws+"/"+room] = b
	}
	return out, rows.Err()
}
