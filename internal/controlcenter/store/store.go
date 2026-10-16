// Package store is the control center's PostgreSQL state (ADR 0008): users,
// identities, sessions, workspace assignments, the audit log, approvals
// history, web push subscriptions, notification preferences, the inbox cache
// and outstanding SAML requests. Migrations are embedded and run with goose.
package store

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is a connected, migrated database.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to url and runs the migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- users

// User is a person who signed in.
type User struct {
	ID           string
	Handle       string
	Email        string
	DisplayName  string
	Provider     string
	MappedRole   string
	RoleOverride string // empty: none
	Disabled     bool
	CreatedAt    time.Time
	LastLogin    *time.Time
}

// Role is the effective role name: the override, else the mapped role.
func (u User) Role() string {
	if u.RoleOverride != "" {
		return u.RoleOverride
	}
	return u.MappedRole
}

const userCols = `id, handle, email, display_name, provider, mapped_role, coalesce(role_override, ''), disabled, created_at, last_login`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Handle, &u.Email, &u.DisplayName, &u.Provider, &u.MappedRole, &u.RoleOverride, &u.Disabled, &u.CreatedAt, &u.LastLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// Identity is what an identity provider (or the local account) said at sign-in.
type Identity struct {
	Provider    string // local, oidc:<name>, saml:<name>, setup
	Subject     string
	Handle      string // preferred handle; made unique
	Email       string
	DisplayName string
	Groups      []string
	MappedRole  string
}

var handleClean = regexp.MustCompile(`[^a-z0-9-]+`)

// HandleFrom turns a username or email into a messaging-safe handle.
func HandleFrom(s string) string {
	s = strings.ToLower(s)
	if i := strings.IndexByte(s, '@'); i > 0 {
		s = s[:i]
	}
	s = strings.Trim(handleClean.ReplaceAllString(s, "-"), "-")
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "u-" + s
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.TrimRight(s, "-")
}

// SignIn records a sign-in: it finds or creates the user behind the identity,
// refreshes its details and mapped role, and sets last_login.
func (s *Store) SignIn(ctx context.Context, id Identity) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx)
	var userID string
	err = tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE provider=$1 AND subject=$2`, id.Provider, id.Subject).Scan(&userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		userID = newID()
		handle, err := uniqueHandle(ctx, tx, HandleFrom(id.Handle))
		if err != nil {
			return User{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO users (id, handle, email, display_name, provider, mapped_role, last_login)
			VALUES ($1,$2,$3,$4,$5,$6,now())`, userID, handle, id.Email, id.DisplayName, id.Provider, id.MappedRole); err != nil {
			return User{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identities (provider, subject, user_id, groups) VALUES ($1,$2,$3,$4)`,
			id.Provider, id.Subject, userID, nonNil(id.Groups)); err != nil {
			return User{}, err
		}
	case err != nil:
		return User{}, err
	default:
		if _, err := tx.Exec(ctx, `UPDATE users SET email=$2, display_name=$3, mapped_role=$4, last_login=now() WHERE id=$1`,
			userID, id.Email, id.DisplayName, id.MappedRole); err != nil {
			return User{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE identities SET groups=$3, updated_at=now() WHERE provider=$1 AND subject=$2`,
			id.Provider, id.Subject, nonNil(id.Groups)); err != nil {
			return User{}, err
		}
	}
	u, err := scanUser(tx.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, userID))
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit(ctx)
}

func uniqueHandle(ctx context.Context, tx pgx.Tx, base string) (string, error) {
	for i := 1; i < 1000; i++ {
		h := base
		if i > 1 {
			h = fmt.Sprintf("%s-%d", base, i)
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE handle=$1`, h).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return h, nil
		}
	}
	return "", errors.New("no free handle")
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// GetUser returns a user by ID.
func (s *Store) GetUser(ctx context.Context, id string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

// ListUsers returns every user, by handle.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY handle`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetRoleOverride sets (or, with "", clears) a user's role override.
func (s *Store) SetRoleOverride(ctx context.Context, id, role string) error {
	var v any
	if role != "" {
		v = role
	}
	return execOne(ctx, s.pool, `UPDATE users SET role_override=$2 WHERE id=$1`, id, v)
}

// SetDisabled disables or enables a user; disabling ends their sessions.
func (s *Store) SetDisabled(ctx context.Context, id string, disabled bool) error {
	if err := execOne(ctx, s.pool, `UPDATE users SET disabled=$2 WHERE id=$1`, id, disabled); err != nil {
		return err
	}
	if disabled {
		_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, id)
		return err
	}
	return nil
}

func execOne(ctx context.Context, p *pgxpool.Pool, q string, args ...any) error {
	tag, err := p.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------- workspaces

// Assignments returns the workspaces a user owns or is assigned to.
func (s *Store) Assignments(ctx context.Context, userID string) ([]string, error) {
	return s.strings(ctx, `SELECT workspace FROM workspace_assignments WHERE user_id=$1 ORDER BY workspace`, userID)
}

// SetAssignments replaces a user's member assignments; ownerships stay.
func (s *Store) SetAssignments(ctx context.Context, userID string, workspaces []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_assignments WHERE user_id=$1 AND kind='member'`, userID); err != nil {
		return err
	}
	for _, ws := range workspaces {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_assignments (user_id, workspace, kind) VALUES ($1,$2,'member')
			ON CONFLICT DO NOTHING`, userID, ws); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AddOwner records that a user owns a workspace.
func (s *Store) AddOwner(ctx context.Context, userID, workspace string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO workspace_assignments (user_id, workspace, kind) VALUES ($1,$2,'owner')
		ON CONFLICT (user_id, workspace) DO UPDATE SET kind='owner'`, userID, workspace)
	return err
}

// WorkspaceUsers returns the users assigned to or owning a workspace.
func (s *Store) WorkspaceUsers(ctx context.Context, workspace string) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.handle, u.email, u.display_name, u.provider, u.mapped_role, coalesce(u.role_override, ''), u.disabled, u.created_at, u.last_login FROM users u
		JOIN workspace_assignments a ON a.user_id=u.id WHERE a.workspace=$1 AND NOT u.disabled`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---------------------------------------------------------------- sessions

// Session is a server-side session; the cookie holds the ID, the table its hash.
type Session struct {
	IDHash    []byte
	UserID    string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
	SourceIP  string
	UserAgent string
}

// CreateSession stores a session.
func (s *Store) CreateSession(ctx context.Context, ss Session) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (id_hash, user_id, created_at, last_seen, expires_at, source_ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, ss.IDHash, ss.UserID, ss.CreatedAt, ss.LastSeen, ss.ExpiresAt, ss.SourceIP, ss.UserAgent)
	return err
}

// GetSession returns a session by ID hash.
func (s *Store) GetSession(ctx context.Context, idHash []byte) (Session, error) {
	var ss Session
	err := s.pool.QueryRow(ctx, `SELECT id_hash, user_id, created_at, last_seen, expires_at, source_ip, user_agent
		FROM sessions WHERE id_hash=$1`, idHash).Scan(&ss.IDHash, &ss.UserID, &ss.CreatedAt, &ss.LastSeen, &ss.ExpiresAt, &ss.SourceIP, &ss.UserAgent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ss, ErrNotFound
	}
	return ss, err
}

// TouchSession updates a session's last use.
func (s *Store) TouchSession(ctx context.Context, idHash []byte, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen=$2 WHERE id_hash=$1`, idHash, at)
	return err
}

// DeleteSession removes a session.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id_hash=$1`, idHash)
	return err
}

// DeleteExpiredSessions removes sessions past their absolute or idle limit.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1 OR last_seen <= $2`, now, now.Add(-idle))
	return tag.RowsAffected(), err
}

// ---------------------------------------------------------------- audit

// AuditEvent is one control center action.
type AuditEvent struct {
	At        time.Time `json:"at"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Object    string    `json:"object,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	SourceIP  string    `json:"sourceIP,omitempty"`
	Result    string    `json:"result,omitempty"`
	Before    string    `json:"before,omitempty"`
	After     string    `json:"after,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// InsertAudit appends audit events.
func (s *Store) InsertAudit(ctx context.Context, evs ...AuditEvent) error {
	if len(evs) == 0 {
		return nil
	}
	rows := make([][]any, len(evs))
	for i, e := range evs {
		if e.Result == "" {
			e.Result = "ok"
		}
		rows[i] = []any{e.At, e.Actor, e.Action, e.Object, e.Workspace, e.SourceIP, e.Result, e.Before, e.After, e.Detail}
	}
	_, err := s.pool.CopyFrom(ctx, pgx.Identifier{"audit_log"},
		[]string{"at", "actor", "action", "object", "workspace", "source_ip", "result", "before", "after", "detail"},
		pgx.CopyFromRows(rows))
	return err
}

// AuditFilter selects audit events. Zero fields match everything.
type AuditFilter struct {
	Workspaces []string // nil: all; empty non-nil: none
	Since      time.Time
	Until      time.Time
	Actor      string
	Actions    []string // prefixes
	Limit      int
}

// QueryAudit returns matching events, newest first.
func (s *Store) QueryAudit(ctx context.Context, f AuditFilter) ([]AuditEvent, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Workspaces != nil {
		add("workspace = ANY($%d)", f.Workspaces)
	}
	if !f.Since.IsZero() {
		add("at >= $%d", f.Since)
	}
	if !f.Until.IsZero() {
		add("at < $%d", f.Until)
	}
	if f.Actor != "" {
		add("actor = $%d", f.Actor)
	}
	if len(f.Actions) > 0 {
		var ors []string
		for _, a := range f.Actions {
			args = append(args, strings.ReplaceAll(a, "%", `\%`)+"%")
			ors = append(ors, fmt.Sprintf("action LIKE $%d", len(args)))
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	q := `SELECT at, actor, action, object, workspace, source_ip, result, before, after, detail FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	q += fmt.Sprintf(" ORDER BY at DESC, id DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Object, &e.Workspace, &e.SourceIP, &e.Result, &e.Before, &e.After, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteAuditBefore enforces audit retention.
func (s *Store) DeleteAuditBefore(ctx context.Context, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM audit_log WHERE at < $1`, t)
	return tag.RowsAffected(), err
}

// ---------------------------------------------------------------- approvals

// Approval is an approval request and its outcome.
type Approval struct {
	ID, Workspace, Room, CLI, Tool, Summary, Detail, Reason string
	SessionScopeAvailable                                   bool
	RequestedAt                                             time.Time
	ExpiresAt                                               *time.Time
	State                                                   string // pending, answered, withdrawn, expired
	Decision, AnsweredBy, AnswerMessage                     string
	AnsweredAt                                              *time.Time
}

// UpsertApproval records an approval's latest state.
func (s *Store) UpsertApproval(ctx context.Context, a Approval) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO approvals (id, workspace, room, cli, tool, summary, detail, reason,
		session_scope_available, requested_at, expires_at, state, decision, answered_by, answer_message, answered_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT (id) DO UPDATE SET state=$12, decision=$13, answered_by=$14, answer_message=$15, answered_at=$16, expires_at=$11`,
		a.ID, a.Workspace, a.Room, a.CLI, a.Tool, a.Summary, a.Detail, a.Reason, a.SessionScopeAvailable,
		a.RequestedAt, a.ExpiresAt, a.State, a.Decision, a.AnsweredBy, a.AnswerMessage, a.AnsweredAt)
	return err
}

// ListApprovals returns approvals of the given workspaces (nil: all),
// optionally only one state, newest first.
func (s *Store) ListApprovals(ctx context.Context, workspaces []string, state string, before time.Time, limit int) ([]Approval, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	q := `SELECT id, workspace, room, cli, tool, summary, detail, reason, session_scope_available, requested_at, expires_at,
		state, decision, answered_by, answer_message, answered_at FROM approvals
		WHERE requested_at < $1 AND ($2 = '' OR state = $2) AND ($3::text[] IS NULL OR workspace = ANY($3))
		ORDER BY requested_at DESC LIMIT $4`
	var wsArg any
	if workspaces != nil {
		wsArg = workspaces
	}
	rows, err := s.pool.Query(ctx, q, before, state, wsArg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.Workspace, &a.Room, &a.CLI, &a.Tool, &a.Summary, &a.Detail, &a.Reason,
			&a.SessionScopeAvailable, &a.RequestedAt, &a.ExpiresAt, &a.State, &a.Decision, &a.AnsweredBy, &a.AnswerMessage, &a.AnsweredAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- web push

// PushSubscription is a browser's web push subscription.
type PushSubscription struct {
	Endpoint, UserID, P256dh, Auth, UserAgent string
}

// SavePushSubscription stores (or moves to this user) a subscription.
func (s *Store) SavePushSubscription(ctx context.Context, p PushSubscription) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO webpush_subscriptions (endpoint, user_id, p256dh, auth, user_agent) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (endpoint) DO UPDATE SET user_id=$2, p256dh=$3, auth=$4, user_agent=$5`, p.Endpoint, p.UserID, p.P256dh, p.Auth, p.UserAgent)
	return err
}

// DeletePushSubscription removes a subscription; userID "" removes it for anyone.
func (s *Store) DeletePushSubscription(ctx context.Context, userID, endpoint string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM webpush_subscriptions WHERE endpoint=$1 AND ($2='' OR user_id=$2)`, endpoint, userID)
	return err
}

// PushSubscriptions returns a user's subscriptions.
func (s *Store) PushSubscriptions(ctx context.Context, userID string) ([]PushSubscription, error) {
	rows, err := s.pool.Query(ctx, `SELECT endpoint, user_id, p256dh, auth, user_agent FROM webpush_subscriptions WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.Endpoint, &p.UserID, &p.P256dh, &p.Auth, &p.UserAgent); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- notification prefs

// Pref is a person's channels for one event in one workspace ("" = all).
type Pref struct {
	Workspace, Event string
	Channels         []string
}

// Prefs returns a user's notification preferences.
func (s *Store) Prefs(ctx context.Context, userID string) ([]Pref, error) {
	rows, err := s.pool.Query(ctx, `SELECT workspace, event, channels FROM notification_prefs WHERE user_id=$1 ORDER BY workspace, event`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pref
	for rows.Next() {
		var p Pref
		if err := rows.Scan(&p.Workspace, &p.Event, &p.Channels); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPrefs replaces a user's notification preferences.
func (s *Store) SetPrefs(ctx context.Context, userID string, prefs []Pref) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM notification_prefs WHERE user_id=$1`, userID); err != nil {
		return err
	}
	for _, p := range prefs {
		if _, err := tx.Exec(ctx, `INSERT INTO notification_prefs (user_id, workspace, event, channels) VALUES ($1,$2,$3,$4)
			ON CONFLICT (user_id, workspace, event) DO UPDATE SET channels=$4`, userID, p.Workspace, p.Event, nonNil(p.Channels)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- inbox

// InboxMessage is a message to or from a person.
type InboxMessage struct {
	UserID, ID, Workspace, From, To, Body, ReplyTo string
	SentAt                                         time.Time
	Direction                                      string // in, out
	Read                                           bool
}

// InsertInbox stores a message once; it reports whether it was new.
func (s *Store) InsertInbox(ctx context.Context, m InboxMessage) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO inbox (user_id, id, workspace, from_addr, to_addr, body, reply_to, sent_at, direction, read)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`,
		m.UserID, m.ID, m.Workspace, m.From, m.To, m.Body, m.ReplyTo, m.SentAt, m.Direction, m.Read)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ListInbox returns a user's messages, newest first, and the unread count.
func (s *Store) ListInbox(ctx context.Context, userID, workspace, peer string, before time.Time, limit int) ([]InboxMessage, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id, id, workspace, from_addr, to_addr, body, reply_to, sent_at, direction, read FROM inbox
		WHERE user_id=$1 AND ($2='' OR workspace=$2) AND ($3='' OR from_addr=$3 OR to_addr=$3) AND sent_at < $4
		ORDER BY sent_at DESC LIMIT $5`, userID, workspace, peer, before, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []InboxMessage
	for rows.Next() {
		var m InboxMessage
		if err := rows.Scan(&m.UserID, &m.ID, &m.Workspace, &m.From, &m.To, &m.Body, &m.ReplyTo, &m.SentAt, &m.Direction, &m.Read); err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var unread int
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM inbox WHERE user_id=$1 AND NOT read AND direction='in' AND ($2='' OR workspace=$2)`,
		userID, workspace).Scan(&unread)
	return out, unread, err
}

// MarkRead marks messages read.
func (s *Store) MarkRead(ctx context.Context, userID string, ids []string) error {
	_, err := s.pool.Exec(ctx, `UPDATE inbox SET read=true WHERE user_id=$1 AND id = ANY($2)`, userID, ids)
	return err
}

// ---------------------------------------------------------------- SAML requests

// SaveSAMLRequest remembers an AuthnRequest ID until it expires.
func (s *Store) SaveSAMLRequest(ctx context.Context, id, provider string, expires time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO saml_requests (id, provider, expires_at) VALUES ($1,$2,$3)`, id, provider, expires)
	return err
}

// ConsumeSAMLRequest deletes an unexpired request ID and reports whether it existed.
func (s *Store) ConsumeSAMLRequest(ctx context.Context, id, provider string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM saml_requests WHERE id=$1 AND provider=$2 AND expires_at > now()`, id, provider)
	if err != nil {
		return false, err
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM saml_requests WHERE expires_at <= now()`)
	return tag.RowsAffected() == 1, nil
}
