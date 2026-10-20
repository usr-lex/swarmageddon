package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Activity kinds (docs/design.md §8.7).
const (
	KindMessage  = "message"  // agent to agent
	KindHuman    = "human"    // to or from a person
	KindCrossing = "crossing" // over a mediated link
	KindRefusal  = "refusal"
	KindPost     = "post"    // to a channel; target "#<channel>"
	KindVerdict  = "verdict" // a warden verdict about the actor (a room)

	KindCommit          = "commit"
	KindPROpened        = "pr_opened"
	KindPRMerged        = "pr_merged"
	KindPRClosed        = "pr_closed"
	KindReviewApproved  = "review_approved"
	KindReviewChanges   = "review_changes"
	KindReviewComment   = "review_comment"
	KindIssueOpened     = "issue_opened"
	KindIssueClosed     = "issue_closed"
	KindComment         = "comment"
	KindCIFailed        = "ci_failed"
	KindCIPassed        = "ci_passed"
	KindReviewRequested = "review_requested"
	KindAssigned        = "assigned"
	KindMentioned       = "mentioned"

	KindPresence = "presence"
	KindTask     = "task"
	KindTokens   = "tokens"
)

// EdgeKinds are the kinds drawn as edges between actors.
var EdgeKinds = []string{KindMessage, KindHuman, KindCrossing, KindPost, KindReviewRequested, KindAssigned, KindMentioned}

// statusKinds are not activity in the heatmap's sense.
var statusKinds = []string{KindPresence, KindTask, KindTokens}

// Activity is one observatory event.
type Activity struct {
	ID        int64
	TS        time.Time
	Workspace string
	Actor     string
	Kind      string
	Target    string
	Repo      string
	Number    int
	Summary   string
	RefURL    string
	Value     int64
	Dedup     string // optional natural key
}

// InsertActivity stores events, skipping ones whose dedup key exists. It
// returns the events actually stored, with their IDs.
func (s *Store) InsertActivity(ctx context.Context, evs ...Activity) ([]Activity, error) {
	var out []Activity
	var errs []error
	for _, e := range evs {
		var dedup any
		if e.Dedup != "" {
			dedup = e.Dedup
		}
		err := s.pool.QueryRow(ctx, `INSERT INTO activity_events (ts, workspace, actor, kind, target, repo, number, summary, ref_url, value, dedup)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (workspace, dedup) WHERE dedup IS NOT NULL DO NOTHING RETURNING id`,
			e.TS, e.Workspace, noNUL(e.Actor), e.Kind, noNUL(e.Target), noNUL(e.Repo), e.Number, noNUL(truncate(e.Summary, 500)), noNUL(e.RefURL), e.Value, dedup).Scan(&e.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			// The rest of the batch is still stored; the caller retries the failed ones.
			errs = append(errs, &InsertError{Event: e, Err: err})
			continue
		}
		out = append(out, e)
	}
	return out, errors.Join(errs...)
}

// InsertError is an event InsertActivity couldn't store.
type InsertError struct {
	Event Activity
	Err   error
}

func (e *InsertError) Error() string { return fmt.Sprintf("activity %s at %s: %v", e.Event.Kind, e.Event.TS.Format(time.RFC3339), e.Err) }
func (e *InsertError) Unwrap() error { return e.Err }

// noNUL drops NUL characters, which PostgreSQL text can't hold.
func noNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// DeleteActivityBefore enforces a workspace's retention.
func (s *Store) DeleteActivityBefore(ctx context.Context, workspace string, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM activity_events WHERE workspace=$1 AND ts < $2`, workspace, t)
	return tag.RowsAffected(), err
}

// Edge is a count of events from one actor to another of one kind.
type Edge struct {
	From, To, Kind string
	Count          int64
}

// Edges counts who talked to (or handed work to) whom.
func (s *Store) Edges(ctx context.Context, ws string, since, until time.Time) ([]Edge, error) {
	rows, err := s.pool.Query(ctx, `SELECT actor, target, kind, count(*) FROM activity_events
		WHERE workspace=$1 AND ts >= $2 AND ts < $3 AND kind = ANY($4) AND target <> ''
		GROUP BY 1,2,3 ORDER BY 4 DESC, 1, 2`, ws, since, until, EdgeKinds)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Edge, error) {
		var e Edge
		return e, r.Scan(&e.From, &e.To, &e.Kind, &e.Count)
	})
}

// NowRow is an agent's latest state.
type NowRow struct {
	Actor         string
	Presence      string
	PresenceSince time.Time
	Task          string
	ItemRepo      string
	ItemNumber    int
	ItemTitle     string
	ItemURL       string
	ItemAt        time.Time
}

// Now returns each actor's latest presence, task and touched item.
func (s *Store) Now(ctx context.Context, ws string) ([]NowRow, error) {
	byActor := map[string]*NowRow{}
	var order []string
	get := func(a string) *NowRow {
		if r, ok := byActor[a]; ok {
			return r
		}
		r := &NowRow{Actor: a}
		byActor[a] = r
		order = append(order, a)
		return r
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (actor, kind) actor, kind, summary, ts FROM activity_events
		WHERE workspace=$1 AND kind IN ('presence','task') ORDER BY actor, kind, ts DESC`, ws)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var actor, kind, summary string
		var ts time.Time
		if err := rows.Scan(&actor, &kind, &summary, &ts); err != nil {
			rows.Close()
			return nil, err
		}
		r := get(actor)
		if kind == KindPresence {
			r.Presence, r.PresenceSince = summary, ts
		} else {
			r.Task = summary
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT DISTINCT ON (actor) actor, repo, number, summary, ref_url, ts FROM activity_events
		WHERE workspace=$1 AND number > 0 ORDER BY actor, ts DESC`, ws)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var actor string
		var it NowRow
		if err := rows.Scan(&actor, &it.ItemRepo, &it.ItemNumber, &it.ItemTitle, &it.ItemURL, &it.ItemAt); err != nil {
			rows.Close()
			return nil, err
		}
		r := get(actor)
		r.ItemRepo, r.ItemNumber, r.ItemTitle, r.ItemURL, r.ItemAt = it.ItemRepo, it.ItemNumber, it.ItemTitle, it.ItemURL, it.ItemAt
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]NowRow, 0, len(order))
	for _, a := range order {
		out = append(out, *byActor[a])
	}
	return out, nil
}

// DayCount is a count for one actor on one UTC day.
type DayCount struct {
	Actor string
	Day   time.Time
	Count int64
}

// Heatmap counts activity per actor and day (status updates excluded).
func (s *Store) Heatmap(ctx context.Context, ws, actor string, since time.Time) ([]DayCount, error) {
	rows, err := s.pool.Query(ctx, `SELECT actor, (ts AT TIME ZONE 'UTC')::date AS day, count(*) FROM activity_events
		WHERE workspace=$1 AND ts >= $2 AND ($3 = '' OR actor = $3) AND NOT (kind = ANY($4))
		GROUP BY 1,2 ORDER BY 2,1`, ws, since, actor, statusKinds)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (DayCount, error) {
		var d DayCount
		return d, r.Scan(&d.Actor, &d.Day, &d.Count)
	})
}

// AxesRow is an agent's activity profile.
type AxesRow struct {
	Actor                                            string
	Commits, PullRequests, Reviews, Issues, Messages int64
}

// Axes counts each actor's commits, PRs, reviews, issues and messages.
func (s *Store) Axes(ctx context.Context, ws string, since time.Time) ([]AxesRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT actor,
			count(*) FILTER (WHERE kind = 'commit'),
			count(*) FILTER (WHERE kind = 'pr_opened'),
			count(*) FILTER (WHERE kind IN ('review_approved','review_changes','review_comment')),
			count(*) FILTER (WHERE kind IN ('issue_opened','issue_closed')),
			count(*) FILTER (WHERE kind IN ('message','human','crossing','post'))
		FROM activity_events WHERE workspace=$1 AND ts >= $2 AND NOT (kind = ANY($3))
		GROUP BY actor ORDER BY actor`, ws, since, statusKinds)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AxesRow, error) {
		var a AxesRow
		return a, r.Scan(&a.Actor, &a.Commits, &a.PullRequests, &a.Reviews, &a.Issues, &a.Messages)
	})
}

// TimelineFilter selects timeline events.
type TimelineFilter struct {
	Actors   []string
	Kinds    []string
	Repo     string
	BeforeID int64 // page: events with smaller IDs
	Limit    int
}

// Timeline returns events newest first.
func (s *Store) Timeline(ctx context.Context, ws string, f TimelineFilter) ([]Activity, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	var actors, kinds any
	if len(f.Actors) > 0 {
		actors = f.Actors
	}
	if len(f.Kinds) > 0 {
		kinds = f.Kinds
	}
	rows, err := s.pool.Query(ctx, `SELECT id, ts, workspace, actor, kind, target, repo, number, summary, ref_url, value, coalesce(dedup, '')
		FROM activity_events WHERE workspace=$1 AND ($2::text[] IS NULL OR actor = ANY($2)) AND ($3::text[] IS NULL OR kind = ANY($3))
		AND ($4 = '' OR repo = $4) AND ($5 = 0 OR id < $5) ORDER BY id DESC LIMIT $6`, ws, actors, kinds, f.Repo, f.BeforeID, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Activity, error) {
		var a Activity
		return a, r.Scan(&a.ID, &a.TS, &a.Workspace, &a.Actor, &a.Kind, &a.Target, &a.Repo, &a.Number, &a.Summary, &a.RefURL, &a.Value, &a.Dedup)
	})
}

// ThroughputRow is one actor's (or, for latency, the swarm's) numbers on one day.
type ThroughputRow struct {
	Day        time.Time
	Actor      string
	Merged     int64
	CIFailures int64
	Tokens     int64
}

// LatencyRow is the median time from PR opened to first review, by day of the review.
type LatencyRow struct {
	Day           time.Time
	MedianSeconds float64
	Reviews       int64
}

// Throughput returns per day and actor: PRs merged (credited to the PR's
// author), CI failures and tokens; and the daily median review latency.
func (s *Store) Throughput(ctx context.Context, ws string, since time.Time) ([]ThroughputRow, []LatencyRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT (ts AT TIME ZONE 'UTC')::date AS day,
			CASE WHEN kind = 'pr_merged' AND target <> '' THEN target ELSE actor END AS who,
			count(*) FILTER (WHERE kind = 'pr_merged'),
			count(*) FILTER (WHERE kind = 'ci_failed'),
			coalesce(sum(value) FILTER (WHERE kind = 'tokens'), 0)
		FROM activity_events WHERE workspace=$1 AND ts >= $2 AND kind IN ('pr_merged','ci_failed','tokens')
		GROUP BY 1,2 ORDER BY 1,2`, ws, since)
	if err != nil {
		return nil, nil, err
	}
	tp, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ThroughputRow, error) {
		var t ThroughputRow
		return t, r.Scan(&t.Day, &t.Actor, &t.Merged, &t.CIFailures, &t.Tokens)
	})
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.pool.Query(ctx, `WITH opened AS (
			SELECT repo, number, min(ts) AS ts FROM activity_events WHERE workspace=$1 AND kind='pr_opened' GROUP BY 1,2),
		first_review AS (
			SELECT repo, number, min(ts) AS ts FROM activity_events
			WHERE workspace=$1 AND kind IN ('review_approved','review_changes','review_comment') GROUP BY 1,2)
		SELECT (r.ts AT TIME ZONE 'UTC')::date AS day,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM r.ts - o.ts)), count(*)
		FROM opened o JOIN first_review r USING (repo, number)
		WHERE r.ts >= $2 AND r.ts >= o.ts GROUP BY 1 ORDER BY 1`, ws, since)
	if err != nil {
		return nil, nil, err
	}
	lat, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LatencyRow, error) {
		var l LatencyRow
		return l, r.Scan(&l.Day, &l.MedianSeconds, &l.Reviews)
	})
	return tp, lat, err
}

// Report is a chronicler's report.
type Report struct {
	ID        int64
	Workspace string
	Sender    string
	TS        time.Time
	Body      string
	MessageID string
}

// InsertReport stores a report once (by message ID); it reports whether it was new.
func (s *Store) InsertReport(ctx context.Context, r Report) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO reports (workspace, sender, ts, body, message_id) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (workspace, message_id) DO NOTHING`, r.Workspace, r.Sender, r.TS, r.Body, r.MessageID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Reports returns a workspace's reports, newest first.
func (s *Store) Reports(ctx context.Context, ws string, limit int) ([]Report, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT id, workspace, sender, ts, body, message_id FROM reports WHERE workspace=$1 ORDER BY ts DESC, id DESC LIMIT $2`, ws, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Report, error) {
		var x Report
		return x, r.Scan(&x.ID, &x.Workspace, &x.Sender, &x.TS, &x.Body, &x.MessageID)
	})
}

// ActivityCursor returns how far a workspace's messaging audit was followed.
func (s *Store) ActivityCursor(ctx context.Context, ws string) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `SELECT ts FROM activity_cursors WHERE workspace=$1`, ws).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

// SetActivityCursor records how far the messaging audit was followed.
func (s *Store) SetActivityCursor(ctx context.Context, ws string, t time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO activity_cursors (workspace, ts) VALUES ($1,$2)
		ON CONFLICT (workspace) DO UPDATE SET ts = greatest(activity_cursors.ts, $2)`, ws, t)
	return err
}

// MarkCrossing records that a message crossed a mediated link: it turns the
// message's event into a crossing, or inserts one if the message isn't there.
func (s *Store) MarkCrossing(ctx context.Context, c Activity) error {
	tag, err := s.pool.Exec(ctx, `UPDATE activity_events SET kind=$3, summary=$4 WHERE workspace=$1 AND dedup=$2`,
		c.Workspace, c.Dedup, KindCrossing, c.Summary)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		c.Kind = KindCrossing
		_, err = s.InsertActivity(ctx, c)
	}
	return err
}
