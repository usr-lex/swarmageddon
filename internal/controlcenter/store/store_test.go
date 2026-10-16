package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store/storetest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Stop()
	os.Exit(code)
}

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestHandleFrom(t *testing.T) {
	for in, want := range map[string]string{
		"Alice@Example.com": "alice",
		"bob.smith":         "bob-smith",
		"123":               "u-123",
		"":                  "u",
	} {
		if got := store.HandleFrom(in); got != want {
			t.Errorf("HandleFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStore(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	u1, err := s.SignIn(ctx, store.Identity{Provider: "oidc:kc", Subject: "s1", Handle: "alice@x", Email: "alice@x", MappedRole: "viewer", Groups: []string{"g"}})
	if err != nil {
		t.Fatal(err)
	}
	u2, err := s.SignIn(ctx, store.Identity{Provider: "saml:kc", Subject: "s2", Handle: "alice@y", MappedRole: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if u1.Handle != "alice" || u2.Handle != "alice-2" {
		t.Fatalf("handles %q %q", u1.Handle, u2.Handle)
	}
	// Signing in again updates, not duplicates.
	again, err := s.SignIn(ctx, store.Identity{Provider: "oidc:kc", Subject: "s1", Handle: "alice@x", MappedRole: "admin"})
	if err != nil || again.ID != u1.ID || again.Role() != "admin" {
		t.Fatalf("re-sign-in: %+v %v", again, err)
	}
	if err := s.SetRoleOverride(ctx, u1.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetUser(ctx, u1.ID); u.Role() != "viewer" {
		t.Fatalf("override not applied: %+v", u)
	}

	// Assignments: members replaced, owners kept.
	if err := s.AddOwner(ctx, u1.ID, "acme"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAssignments(ctx, u1.ID, []string{"beta"}); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.Assignments(ctx, u1.ID)
	if len(ws) != 2 || ws[0] != "acme" || ws[1] != "beta" {
		t.Fatalf("assignments %v", ws)
	}
	users, _ := s.WorkspaceUsers(ctx, "acme")
	if len(users) != 1 || users[0].ID != u1.ID {
		t.Fatalf("workspace users %+v", users)
	}

	// Sessions.
	now := time.Now().UTC().Truncate(time.Second)
	ss := store.Session{IDHash: []byte("h1"), UserID: u1.ID, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(ctx, ss); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSession(ctx, []byte("h1")); err != nil || got.UserID != u1.ID {
		t.Fatalf("get session: %+v %v", got, err)
	}
	if n, _ := s.DeleteExpiredSessions(ctx, now.Add(2*time.Hour), 12*time.Hour); n != 1 {
		t.Fatalf("expired sessions deleted: %d", n)
	}
	if err := s.SetDisabled(ctx, u2.ID, true); err != nil {
		t.Fatal(err)
	}

	// Audit with retention.
	old := store.AuditEvent{At: now.Add(-48 * time.Hour), Actor: "alice", Action: "room.pause", Workspace: "acme"}
	cur := store.AuditEvent{At: now, Actor: "bob", Action: "workspace.apply", Workspace: "beta"}
	if err := s.InsertAudit(ctx, old, cur); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryAudit(ctx, store.AuditFilter{Actions: []string{"room."}})
	if err != nil || len(got) != 1 || got[0].Actor != "alice" {
		t.Fatalf("audit query: %+v %v", got, err)
	}
	got, _ = s.QueryAudit(ctx, store.AuditFilter{Workspaces: []string{}})
	if len(got) != 0 {
		t.Fatalf("empty workspace filter matched %d", len(got))
	}
	if n, _ := s.DeleteAuditBefore(ctx, now.Add(-time.Hour)); n != 1 {
		t.Fatalf("retention deleted %d", n)
	}

	// Approvals.
	a := store.Approval{ID: "a1", Workspace: "acme", Room: "athena", Tool: "Bash", RequestedAt: now, State: "pending"}
	if err := s.UpsertApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.State, a.Decision, a.AnsweredBy = "answered", "allow", "alice@acme"
	if err := s.UpsertApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListApprovals(ctx, []string{"acme"}, "", time.Time{}, 10)
	if err != nil || len(list) != 1 || list[0].State != "answered" {
		t.Fatalf("approvals %+v %v", list, err)
	}
	list, _ = s.ListApprovals(ctx, nil, "pending", time.Time{}, 10)
	if len(list) != 0 {
		t.Fatalf("pending approvals %+v", list)
	}

	// Push subscriptions and prefs.
	if err := s.SavePushSubscription(ctx, store.PushSubscription{Endpoint: "https://push/1", UserID: u1.ID, P256dh: "k", Auth: "a"}); err != nil {
		t.Fatal(err)
	}
	subs, _ := s.PushSubscriptions(ctx, u1.ID)
	if len(subs) != 1 {
		t.Fatalf("subs %+v", subs)
	}
	if err := s.SetPrefs(ctx, u1.ID, []store.Pref{{Event: "ciRed", Channels: []string{"webPush"}}}); err != nil {
		t.Fatal(err)
	}
	prefs, _ := s.Prefs(ctx, u1.ID)
	if len(prefs) != 1 || prefs[0].Channels[0] != "webPush" {
		t.Fatalf("prefs %+v", prefs)
	}

	// Inbox: insert once.
	m := store.InboxMessage{UserID: u1.ID, ID: "m1", Workspace: "acme", From: "athena@acme", To: "alice@acme", Body: "hi", SentAt: now, Direction: "in"}
	if isNew, err := s.InsertInbox(ctx, m); err != nil || !isNew {
		t.Fatalf("insert inbox %v %v", isNew, err)
	}
	if isNew, _ := s.InsertInbox(ctx, m); isNew {
		t.Fatal("duplicate inbox insert")
	}
	msgs, unread, err := s.ListInbox(ctx, u1.ID, "acme", "", time.Time{}, 10)
	if err != nil || len(msgs) != 1 || unread != 1 {
		t.Fatalf("inbox %+v %d %v", msgs, unread, err)
	}
	_ = s.MarkRead(ctx, u1.ID, []string{"m1"})
	if _, unread, _ = s.ListInbox(ctx, u1.ID, "", "", time.Time{}, 10); unread != 0 {
		t.Fatalf("unread after mark %d", unread)
	}

	// SAML request IDs are single use.
	if err := s.SaveSAMLRequest(ctx, "_r1", "kc", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ConsumeSAMLRequest(ctx, "_r1", "kc"); !ok {
		t.Fatal("first consume failed")
	}
	if ok, _ := s.ConsumeSAMLRequest(ctx, "_r1", "kc"); ok {
		t.Fatal("replayed request ID accepted")
	}
}
