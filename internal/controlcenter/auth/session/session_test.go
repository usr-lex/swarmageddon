package session

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

type fakeDB struct {
	mu sync.Mutex
	m  map[string]store.Session
}

func (f *fakeDB) CreateSession(_ context.Context, s store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[string(s.IDHash)] = s
	return nil
}
func (f *fakeDB) GetSession(_ context.Context, h []byte) (store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.m[string(h)]
	if !ok {
		return s, store.ErrNotFound
	}
	return s, nil
}
func (f *fakeDB) TouchSession(_ context.Context, h []byte, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.m[string(h)]
	s.LastSeen = at
	f.m[string(h)] = s
	return nil
}
func (f *fakeDB) DeleteSession(_ context.Context, h []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, string(h))
	return nil
}

func cookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

func req(c *http.Cookie) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if c != nil {
		r.AddCookie(c)
	}
	return r
}

func TestStatelessAndPromotion(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m, err := NewManager(bytes.Repeat([]byte("k"), 32), true)
	if err != nil {
		t.Fatal(err)
	}
	m.Now = func() time.Time { return now }
	ctx := context.Background()

	rec := httptest.NewRecorder()
	if _, err := m.Create(ctx, rec, req(nil), Data{Provider: "local", Subject: "admin", Handle: "admin", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	c := cookieFrom(t, rec)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || !strings.HasPrefix(c.Value, "s1.") {
		t.Fatalf("cookie attributes: %+v", c)
	}
	d, err := m.Load(ctx, httptest.NewRecorder(), req(c))
	if err != nil || d.Role != "admin" {
		t.Fatalf("load: %+v %v", d, err)
	}

	// Tampering is refused.
	bad := *c
	bad.Value = strings.Replace(c.Value, "s1.e", "s1.f", 1) + "x"
	if _, err := m.Load(ctx, nil, req(&bad)); err == nil {
		t.Fatal("tampered cookie accepted")
	}

	// Idle timeout.
	now = now.Add(13 * time.Hour)
	if _, err := m.Load(ctx, nil, req(c)); err == nil {
		t.Fatal("idle session accepted")
	}
	now = now.Add(-13 * time.Hour)

	// Once the database is up, the stateless session moves into it.
	db := &fakeDB{m: map[string]store.Session{}}
	m.DB = func() DB { return db }
	m.Promote = func(_ context.Context, d *Data) (string, error) { return "user-1", nil }
	rec = httptest.NewRecorder()
	d, err = m.Load(ctx, rec, req(c))
	if err != nil || d.UserID != "user-1" || d.ID == "" {
		t.Fatalf("promote: %+v %v", d, err)
	}
	dbc := cookieFrom(t, rec)
	if !strings.HasPrefix(dbc.Value, "d1.") || len(db.m) != 1 {
		t.Fatalf("db cookie %q, %d sessions", dbc.Value, len(db.m))
	}
	// The old signed cookie no longer works.
	if _, err := m.Load(ctx, nil, req(c)); err == nil {
		t.Fatal("promoted stateless cookie still accepted")
	}
	if d, err := m.Load(ctx, nil, req(dbc)); err != nil || d.UserID != "user-1" {
		t.Fatalf("db load: %+v %v", d, err)
	}

	// Absolute expiry.
	now = now.Add(8 * 24 * time.Hour)
	if _, err := m.Load(ctx, nil, req(dbc)); err == nil {
		t.Fatal("expired db session accepted")
	}
	if len(db.m) != 0 {
		t.Fatal("expired session not deleted")
	}
}

func TestDestroy(t *testing.T) {
	m, _ := NewManager(bytes.Repeat([]byte("k"), 32), false)
	ctx := context.Background()
	rec := httptest.NewRecorder()
	_, _ = m.Create(ctx, rec, req(nil), Data{Provider: "local", Subject: "a", Handle: "a", Role: "admin"})
	c := cookieFrom(t, rec)
	m.Destroy(ctx, httptest.NewRecorder(), req(c))
	if _, err := m.Load(ctx, nil, req(c)); err == nil {
		t.Fatal("logged-out stateless session accepted")
	}
}

func TestSeal(t *testing.T) {
	m, _ := NewManager(bytes.Repeat([]byte("k"), 32), false)
	s, err := m.Seal("oidc", map[string]string{"state": "x"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := m.Unseal("oidc", s, &v); err != nil || v["state"] != "x" {
		t.Fatalf("unseal: %v %v", v, err)
	}
	if err := m.Unseal("saml", s, &v); err == nil {
		t.Fatal("sealed value accepted for another purpose")
	}
	m.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if err := m.Unseal("oidc", s, &v); err == nil {
		t.Fatal("expired sealed value accepted")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Fatalf("behind ingress: %q", got)
	}
	r.RemoteAddr = "198.51.100.7:1234"
	if got := ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("direct client must not be able to spoof: %q", got)
	}
}
