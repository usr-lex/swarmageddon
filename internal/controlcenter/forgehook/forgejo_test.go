package forgehook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// TestRealForgejo runs against a real Forgejo when FORGEJO_TEST_URL and
// FORGEJO_TEST_TOKEN (an admin token with all scopes) are set. With
// FORGEJO_TEST_HOOK_HOST (the address Forgejo reaches this test on, e.g.
// host.docker.internal; Forgejo needs [webhook] ALLOWED_HOST_LIST to allow it)
// it also checks deliveries end to end.
func TestRealForgejo(t *testing.T) {
	base, token := os.Getenv("FORGEJO_TEST_URL"), os.Getenv("FORGEJO_TEST_TOKEN")
	if base == "" || token == "" {
		t.Skip("set FORGEJO_TEST_URL and FORGEJO_TEST_TOKEN to run against a real Forgejo")
	}
	ctx := context.Background()
	sfx := fmt.Sprint(time.Now().UnixNano() % 1e8)
	org, reader, outsider := "cc"+sfx, "rd"+sfx, "out"+sfx
	api := func(method, path, sudo string, body any) (int, map[string]any) {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req, _ := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/api/v1"+path, rd)
		req.Header.Set("Authorization", "token "+token)
		req.Header.Set("Content-Type", "application/json")
		if sudo != "" {
			req.Header.Set("Sudo", sudo)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	must := func(code int, out map[string]any) map[string]any {
		t.Helper()
		if code >= 300 {
			t.Fatalf("forgejo: %d %v", code, out)
		}
		return out
	}
	for _, u := range []string{reader, outsider} {
		must(api("POST", "/admin/users", "", map[string]any{"username": u, "email": u + "@agents.test.invalid",
			"password": "Pw-" + u + "-123456", "must_change_password": false}))
	}
	must(api("POST", "/orgs", "", map[string]any{"username": org, "visibility": "private"}))
	team := must(api("POST", "/orgs/"+org+"/teams", "", map[string]any{"name": "implementers", "permission": "write",
		"units": []string{"repo.code", "repo.issues", "repo.pulls"}, "includes_all_repositories": true, "can_create_org_repo": false}))
	teamID := int(team["id"].(float64))
	must(api("PUT", fmt.Sprintf("/teams/%d/members/%s", teamID, reader), "", nil))
	must(api("POST", "/orgs/"+org+"/repos", "", map[string]any{"name": "api", "auto_init": true, "private": true}))

	perms := &ForgeReader{BaseURL: base, Token: func(context.Context) (string, error) { return token, nil }}
	if ok, err := perms.CanRead(ctx, org, "api", reader); err != nil || !ok {
		t.Fatalf("team member can't read: %v %v", ok, err)
	}
	if ok, err := perms.CanRead(ctx, org, "api", outsider); err != nil || ok {
		t.Fatalf("outsider can read: %v %v", ok, err)
	}
	if ok, err := perms.CanRead(ctx, org, "api", "no-such-user-"+sfx); err != nil || ok {
		t.Fatalf("missing user: %v %v", ok, err)
	}

	hookHost := os.Getenv("FORGEJO_TEST_HOOK_HOST")
	if hookHost == "" {
		t.Log("FORGEJO_TEST_HOOK_HOST not set; skipping webhook delivery")
		return
	}
	s := &fakeSender{}
	h := &Handler{
		Dir: &dirWithSecret{fakeDir: fakeDir{ws: v1alpha1.Workspace{Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: org}}},
			rooms: []string{reader, outsider}}, secret: "hook-" + sfx},
		Perms: perms,
		Send:  s,
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	must(api("POST", "/orgs/"+org+"/hooks", "", map[string]any{"type": "forgejo", "active": true,
		"config": map[string]string{"url": fmt.Sprintf("http://%s:%d/hooks/forgejo/ws", hookHost, port), "content_type": "json", "secret": "hook-" + sfx},
		"events": []string{"issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_comment", "workflow_run_failure"}}))
	// The admin opens an issue for reader and mentions the outsider, who can't read the repo.
	must(api("POST", "/repos/"+org+"/api/issues", "", map[string]any{"title": "Real hook", "body": "for @" + reader + " and @" + outsider,
		"assignees": []string{reader}}))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.msgs)
		s.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (assigned + mentioned): %+v", len(s.msgs), s.msgs)
	}
	for _, m := range s.msgs {
		if m.to != reader+"@ws" {
			t.Fatalf("message to %s; only %s may hear about the repository", m.to, reader)
		}
	}
}

type dirWithSecret struct {
	fakeDir
	secret string
}

func (d *dirWithSecret) Secret(context.Context, string) ([]byte, error) { return []byte(d.secret), nil }
