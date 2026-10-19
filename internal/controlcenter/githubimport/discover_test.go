package githubimport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A GitHub organization's repositories and bot-<name> accounts (members and
// contributors), read with a read-only token over paginated REST calls.
func TestDiscover(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		switch r.URL.Path {
		case "/orgs/acme/repos":
			if page == "2" {
				_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "web", "private": true, "has_wiki": false, "open_issues_count": 0, "archived": true}})
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/repos?per_page=100&page=2>; rel="next"`, "http://"+r.Host))
			_ = json.NewEncoder(w).Encode([]map[string]any{{"name": "api", "private": false, "has_wiki": true, "open_issues_count": 12, "description": "the API"}})
		case "/orgs/acme/members":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"login": "bot-athena", "id": 11}, {"login": "alice", "id": 1}})
		case "/repos/acme/api/contributors":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"login": "bot-hermes", "id": 22}, {"login": "bot-athena", "id": 11}, {"login": "dependabot[bot]", "id": 99}})
		case "/repos/acme/web/contributors":
			w.WriteHeader(204) // empty repository
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "github_pat_x", HTTP: srv.Client()}
	d, err := c.Discover(context.Background(), "acme", "bot-")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Repos) != 2 || d.Repos[0].Name != "api" || d.Repos[0].OpenIssues != 12 || !d.Repos[1].Archived {
		t.Errorf("repos %+v", d.Repos)
	}
	if got := fmt.Sprint(d.Bots); got != "[{bot-athena 11 athena} {bot-hermes 22 hermes}]" {
		t.Errorf("bots %s", got)
	}
	if d.People != 1 {
		t.Errorf("people %d (dependabot[bot] is a bot, alice a person)", d.People)
	}
	for _, a := range auth {
		if a != "Bearer github_pat_x" {
			t.Fatalf("authorization %q", a)
		}
	}

	if _, err := c.Discover(context.Background(), "nope", "bot-"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing org: %v", err)
	}
}

func TestRoomName(t *testing.T) {
	for in, want := range map[string]string{"bot-athena": "athena", "bot-Hermes_2": "hermes-2", "bot-": "", "bot-9lives": ""} {
		if got := RoomName(in, "bot-"); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
