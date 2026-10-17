package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
)

// fakeForgejo serves an org's repositories and their open issues (two pages
// for api), checking the reader token.
func fakeForgejo(t *testing.T, calls *atomic.Int32) *httptest.Server {
	issue := func(n int, logins ...string) map[string]any {
		var as []map[string]any
		for _, l := range logins {
			as = append(as, map[string]any{"login": l})
		}
		return map[string]any{"number": n, "state": "open", "assignees": as}
	}
	page1 := make([]map[string]any, 50)
	for i := range page1 {
		page1[i] = issue(i + 1)
	}
	mux := http.NewServeMux()
	reply := func(path string, v any) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Header.Get("Authorization") != "token reader-tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			out := v
			if p := r.URL.Query().Get("page"); r.URL.Path == "/api/v1/repos/acme/api/issues" && p == "2" {
				out = []any{issue(51, "Athena"), map[string]any{"number": 52, "pull_request": map[string]any{}, "assignees": []any{map[string]any{"login": "hermes"}}}}
			}
			if r.URL.Query().Get("state") == "closed" {
				t.Error("asked for closed issues")
			}
			_ = json.NewEncoder(w).Encode(out)
		})
	}
	reply("/api/v1/orgs/acme/repos", []any{map[string]any{"name": "api"}, map[string]any{"name": "wiki"}, map[string]any{"name": "web"}})
	reply("/api/v1/repos/acme/api/issues", page1)
	reply("/api/v1/repos/acme/web/issues", []any{issue(1, "metis")})
	mux.HandleFunc("/api/v1/repos/acme/wiki/issues", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestForgeOpenWork(t *testing.T) {
	var calls atomic.Int32
	s := fakeForgejo(t, &calls)
	now := time.Unix(1000, 0)
	ow := &ForgeOpenWork{
		Forge: func(context.Context) (*forgejo.Client, error) { return forgejo.New(s.URL, "reader-tok"), nil },
		Org: func(_ context.Context, ws string) (string, error) {
			if ws != "acme" {
				return "", fmt.Errorf("no workspace %s", ws)
			}
			return "acme", nil
		},
		Now: func() time.Time { return now },
	}
	ctx := context.Background()
	for room, want := range map[string]bool{"athena": true, "hermes": true, "metis": true, "apollo": false} {
		if got, err := ow.HasOpenWork(ctx, "acme", room); err != nil || got != want {
			t.Errorf("%s: %v %v, want %v", room, got, err, want)
		}
	}
	// One pass for the workspace: repos, api twice, wiki, web.
	if n := calls.Load(); n != 4 {
		t.Fatalf("%d calls", n)
	}
	now = now.Add(DefaultOpenWorkTTL)
	ow.HasOpenWork(ctx, "acme", "athena")
	if n := calls.Load(); n != 8 {
		t.Fatalf("after the TTL: %d calls", n)
	}
	if _, err := ow.HasOpenWork(ctx, "beta", "x"); err == nil {
		t.Fatal("unknown workspace")
	}
}

// The tracker raises idleWithOpenWork from the forge check.
func TestTrackerForgeOpenWork(t *testing.T) {
	var calls atomic.Int32
	s := fakeForgejo(t, &calls)
	ntfy := &hits{}
	ns := ntfy.server(t)
	dir := &fakeDir{settings: v1alpha1.NotificationSettings{Channels: []v1alpha1.NotificationChannel{{Kind: "ntfy", Topic: "acme"}}}}
	ow := &ForgeOpenWork{
		Forge: func(context.Context) (*forgejo.Client, error) { return forgejo.New(s.URL, "reader-tok"), nil },
		Org:   func(context.Context, string) (string, error) { return "acme", nil },
	}
	tr := NewTracker(&Dispatcher{Dir: dir, NtfyURL: ns.URL}, ow)
	now := time.Unix(1000, 0)
	tr.Now = func() time.Time { return now }
	ctx := context.Background()
	tr.Presence(ctx, "acme", "metis", messagingv1.Presence_PRESENCE_IDLE, nil)
	tr.Presence(ctx, "acme", "apollo", messagingv1.Presence_PRESENCE_IDLE, nil)
	now = now.Add(IdleAfter)
	tr.Tick(ctx)
	if ntfy.n() != 1 {
		t.Fatalf("deliveries %q", ntfy.got)
	}
}
