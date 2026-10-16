// Package session issues and checks the control center's session cookies
// (docs/design.md §8.1, ADR 0008).
//
// Before PostgreSQL exists, a session is stateless: the cookie carries the
// session data, signed with HMAC-SHA256 under the key from Secret
// control-center-session. Once the database is up, sessions live in it and
// the cookie carries only a random ID; a signed cookie seen then is moved
// into the database (Promote) and replaced.
//
// Cookies are HttpOnly, Secure (unless in dev mode) and SameSite=Lax. A
// session ends after 12 h without use or 7 days after sign-in.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// CookieName is the session cookie.
const CookieName = "swarm_session"

// Default limits.
const (
	DefaultIdle     = 12 * time.Hour
	DefaultAbsolute = 7 * 24 * time.Hour
)

// ErrNoSession means there is no valid session.
var ErrNoSession = errors.New("no session")

// Data is what a session knows about its person.
type Data struct {
	// UserID is the database user; empty for a stateless session made before
	// the database existed.
	UserID      string    `json:"uid,omitempty"`
	Provider    string    `json:"prv"`
	Subject     string    `json:"sub"`
	Handle      string    `json:"hdl"`
	Email       string    `json:"eml,omitempty"`
	DisplayName string    `json:"dn,omitempty"`
	Groups      []string  `json:"grp,omitempty"`
	Role        string    `json:"rol"` // stateless sessions only
	Created     time.Time `json:"crt"`
	LastSeen    time.Time `json:"lst"`
	Expires     time.Time `json:"exp"`
	// ID is the random ID of a database session (never serialized).
	ID string `json:"-"`
}

// DB is the part of the store sessions need.
type DB interface {
	CreateSession(ctx context.Context, s store.Session) error
	GetSession(ctx context.Context, idHash []byte) (store.Session, error)
	TouchSession(ctx context.Context, idHash []byte, at time.Time) error
	DeleteSession(ctx context.Context, idHash []byte) error
}

// Manager issues and loads sessions.
type Manager struct {
	key      []byte
	Secure   bool
	Idle     time.Duration
	Absolute time.Duration
	Now      func() time.Time
	// DB returns the database, or nil while there is none.
	DB func() DB
	// Promote creates the database user for a stateless session and returns
	// its ID. Called when a signed cookie arrives once the database is up.
	Promote func(ctx context.Context, d *Data) (string, error)

	mu      sync.Mutex
	revoked map[string]time.Time // stateless sessions logged out, by signature, until expiry
}

// NewManager returns a manager signing with key (at least 32 bytes).
func NewManager(key []byte, secure bool) (*Manager, error) {
	if len(key) < 32 {
		return nil, errors.New("session key must have at least 32 bytes")
	}
	return &Manager{key: key, Secure: secure, Idle: DefaultIdle, Absolute: DefaultAbsolute, Now: time.Now, revoked: map[string]time.Time{}}, nil
}

func (m *Manager) db() DB {
	if m.DB == nil {
		return nil
	}
	return m.DB()
}

// Create starts a session for d and sets the cookie.
func (m *Manager) Create(ctx context.Context, w http.ResponseWriter, r *http.Request, d Data) (*Data, error) {
	now := m.Now().UTC()
	d.Created, d.LastSeen, d.Expires = now, now, now.Add(m.Absolute)
	if db := m.db(); db != nil && d.UserID != "" {
		id := randomID()
		if err := db.CreateSession(ctx, store.Session{IDHash: hashID(id), UserID: d.UserID, CreatedAt: d.Created,
			LastSeen: d.LastSeen, ExpiresAt: d.Expires, SourceIP: ClientIP(r), UserAgent: truncate(r.UserAgent(), 300)}); err != nil {
			return nil, err
		}
		d.ID = id
		m.setCookie(w, "d1."+id, d.Expires)
		return &d, nil
	}
	m.setCookie(w, m.sign(d), d.Expires)
	return &d, nil
}

// Load returns the request's session, refreshing its last use. Database
// sessions return Data with only UserID and the times filled in; the caller
// loads the user.
func (m *Manager) Load(ctx context.Context, w http.ResponseWriter, r *http.Request) (*Data, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, ErrNoSession
	}
	now := m.Now().UTC()
	switch {
	case strings.HasPrefix(c.Value, "d1."):
		db := m.db()
		if db == nil {
			return nil, ErrNoSession
		}
		id := strings.TrimPrefix(c.Value, "d1.")
		s, err := db.GetSession(ctx, hashID(id))
		if err != nil {
			return nil, ErrNoSession
		}
		if !now.Before(s.ExpiresAt) || now.Sub(s.LastSeen) >= m.Idle {
			_ = db.DeleteSession(ctx, hashID(id))
			return nil, ErrNoSession
		}
		if now.Sub(s.LastSeen) > time.Minute {
			_ = db.TouchSession(ctx, hashID(id), now)
			s.LastSeen = now
		}
		return &Data{ID: id, UserID: s.UserID, Created: s.CreatedAt, LastSeen: s.LastSeen, Expires: s.ExpiresAt}, nil
	case strings.HasPrefix(c.Value, "s1."):
		d, err := m.verify(c.Value)
		if err != nil {
			return nil, ErrNoSession
		}
		if m.isRevoked(c.Value) || !now.Before(d.Expires) || now.Sub(d.LastSeen) >= m.Idle {
			return nil, ErrNoSession
		}
		if db := m.db(); db != nil && m.Promote != nil && w != nil {
			uid, err := m.Promote(ctx, d)
			if err == nil {
				id := randomID()
				if err := db.CreateSession(ctx, store.Session{IDHash: hashID(id), UserID: uid, CreatedAt: d.Created,
					LastSeen: now, ExpiresAt: d.Expires, SourceIP: ClientIP(r), UserAgent: truncate(r.UserAgent(), 300)}); err == nil {
					m.revoke(c.Value, d.Expires)
					m.setCookie(w, "d1."+id, d.Expires)
					return &Data{ID: id, UserID: uid, Created: d.Created, LastSeen: now, Expires: d.Expires}, nil
				}
			}
		}
		if now.Sub(d.LastSeen) > 5*time.Minute && w != nil {
			d.LastSeen = now
			m.setCookie(w, m.sign(*d), d.Expires)
		}
		return d, nil
	}
	return nil, ErrNoSession
}

// Destroy ends the request's session and clears the cookie.
func (m *Manager) Destroy(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		switch {
		case strings.HasPrefix(c.Value, "d1."):
			if db := m.db(); db != nil {
				_ = db.DeleteSession(ctx, hashID(strings.TrimPrefix(c.Value, "d1.")))
			}
		case strings.HasPrefix(c.Value, "s1."):
			if d, err := m.verify(c.Value); err == nil {
				m.revoke(c.Value, d.Expires)
			}
		}
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: m.Secure, SameSite: http.SameSiteLaxMode})
}

func (m *Manager) setCookie(w http.ResponseWriter, v string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: v, Path: "/", Expires: exp, HttpOnly: true, Secure: m.Secure, SameSite: http.SameSiteLaxMode})
}

func (m *Manager) sign(d Data) string {
	payload, _ := json.Marshal(d)
	p := base64.RawURLEncoding.EncodeToString(payload)
	return "s1." + p + "." + m.mac("session", p)
}

func (m *Manager) verify(v string) (*Data, error) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 || parts[0] != "s1" {
		return nil, ErrNoSession
	}
	if !hmac.Equal([]byte(parts[2]), []byte(m.mac("session", parts[1]))) {
		return nil, ErrNoSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrNoSession
	}
	var d Data
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, ErrNoSession
	}
	return &d, nil
}

// mac is HMAC-SHA256 over purpose and value, so values signed for one
// purpose can't be replayed as another.
func (m *Manager) mac(purpose, v string) string {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(purpose))
	h.Write([]byte{0})
	h.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (m *Manager) revoke(v string, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.Now()
	for k, t := range m.revoked {
		if now.After(t) {
			delete(m.revoked, k)
		}
	}
	m.revoked[v] = until
}

func (m *Manager) isRevoked(v string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.revoked[v]
	return ok
}

// Seal signs v (JSON) for purpose with an expiry, for short-lived values
// such as an OIDC flow's state kept in a cookie.
func (m *Manager) Seal(purpose string, v any, ttl time.Duration) (string, error) {
	payload, err := json.Marshal(struct {
		V   any   `json:"v"`
		Exp int64 `json:"exp"`
	}{v, m.Now().Add(ttl).Unix()})
	if err != nil {
		return "", err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + m.mac(purpose, p), nil
}

// Unseal checks a sealed value and decodes it into v.
func (m *Manager) Unseal(purpose, sealed string, v any) error {
	p, sig, ok := strings.Cut(sealed, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(m.mac(purpose, p))) {
		return errors.New("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return err
	}
	var env struct {
		V   json.RawMessage `json:"v"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if m.Now().Unix() >= env.Exp {
		return errors.New("expired")
	}
	return json.Unmarshal(env.V, v)
}

func randomID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashID(id string) []byte {
	h := sha256.Sum256([]byte(id))
	return h[:]
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ClientIP is the request's client address. The control center sits behind
// the cluster's ingress, so the first X-Forwarded-For entry is used when the
// direct peer is a private address (the ingress controller).
func ClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" && isPrivate(host) {
		first, _, _ := strings.Cut(xff, ",")
		if f := strings.TrimSpace(first); f != "" {
			return f
		}
	}
	return host
}
