package forgehook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
)

// Fixtures were captured from Forgejo 17.0.0 with webhook secret "s3cret":
// org acme, repo acme/api readable by athena and hermes (not metis).
const fixtureSecret = "s3cret"

type fixture struct {
	Event, EventType, Delivery, Signature, Body string
}

func load(t *testing.T, name string) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

type sent struct{ from, id, to, body string }

type fakeSender struct {
	mu   sync.Mutex
	msgs []sent
}

func (f *fakeSender) SendAs(_ context.Context, as, id, to, body, _ string) (*messagingv1.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, sent{as, id, to, body})
	return &messagingv1.SendResponse{Id: id}, nil
}

type fakeDir struct {
	ws    v1alpha1.Workspace
	rooms []string
}

func (f *fakeDir) Workspace(context.Context, string) (*v1alpha1.Workspace, error) { return &f.ws, nil }
func (f *fakeDir) IsRoom(_ context.Context, _, n string) (bool, error) {
	for _, r := range f.rooms {
		if r == n {
			return true, nil
		}
	}
	return false, nil
}
func (f *fakeDir) Secret(context.Context, string) ([]byte, error) { return []byte(fixtureSecret), nil }

type fakePerms map[string]bool

func (f fakePerms) CanRead(_ context.Context, owner, repo, u string) (bool, error) {
	return f[owner+"/"+repo+":"+u], nil
}

func newHandler() (*Handler, *fakeSender, *[]notify.Event) {
	s := &fakeSender{}
	var red []notify.Event
	h := &Handler{
		Dir: &fakeDir{
			ws:    v1alpha1.Workspace{Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: "acme"}}},
			rooms: []string{"athena", "hermes", "metis"},
		},
		Perms: fakePerms{"acme/api:athena": true, "acme/api:hermes": true},
		Send:  s,
		CIRed: func(_ context.Context, ev notify.Event) { red = append(red, ev) },
	}
	return h, s, &red
}

func post(h http.Handler, f fixture, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(f.Body))
	r.Header.Set("X-Forgejo-Event", f.Event)
	r.Header.Set("X-Forgejo-Event-Type", f.EventType)
	r.Header.Set("X-Forgejo-Delivery", f.Delivery)
	r.Header.Set("X-Forgejo-Signature", f.Signature)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRealPayloads(t *testing.T) {
	cases := []struct {
		fixture string
		want    map[string]string // recipient -> start of the body
	}{
		{"issue_assigned", map[string]string{"athena@acme": `hermes assigned you acme/api#1 "Retry uploads"`}},
		// metis is mentioned but can't read the repository.
		{"issue_opened", map[string]string{"athena@acme": `hermes mentioned you in acme/api#1 "Retry uploads"`}},
		// hermes is the issue's author and mentioned: the mention wins; athena is the actor.
		{"issue_comment", map[string]string{"hermes@acme": `athena mentioned you in acme/api#1`}},
		{"pr_opened", nil},
		{"pr_assigned", nil}, // athena assigned herself
		{"pr_review_requested", map[string]string{"hermes@acme": `athena requested your review on acme/api#2 "Retry uploads"`}},
		{"pr_review_rejected", map[string]string{"athena@acme": `hermes requested changes on your pull request acme/api#2`}},
		{"pr_comment", map[string]string{"athena@acme": `hermes commented on acme/api#2`}},
		{"pr_review_approved", map[string]string{"athena@acme": `hermes approved your pull request acme/api#2`}},
	}
	for _, c := range cases {
		t.Run(c.fixture, func(t *testing.T) {
			h, s, _ := newHandler()
			f := load(t, c.fixture)
			if w := post(h, f, "/hooks/forgejo/acme"); w.Code != http.StatusNoContent {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if len(s.msgs) != len(c.want) {
				t.Fatalf("sent %+v, want %v", s.msgs, c.want)
			}
			for _, m := range s.msgs {
				if m.from != "forge@acme" {
					t.Errorf("from %q", m.from)
				}
				if !strings.HasPrefix(m.body, c.want[m.to]) {
					t.Errorf("to %s: body %q, want prefix %q", m.to, m.body, c.want[m.to])
				}
				if !strings.Contains(m.body, "http://") || len(m.id) != 26 {
					t.Errorf("body lacks link or bad id: %q %q", m.body, m.id)
				}
			}
			// Forgejo's retries of the same delivery use the same message IDs.
			s2 := &fakeSender{}
			h.Send = s2
			post(h, f, "/hooks/forgejo/acme")
			for i := range s2.msgs {
				if s2.msgs[i].id != s.msgs[i].id {
					t.Error("retry produced a different message ID")
				}
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	f := load(t, "issue_assigned")
	h, s, _ := newHandler()
	bad := f
	bad.Signature = strings.Repeat("0", 64)
	if w := post(h, bad, "/hooks/forgejo/acme"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", w.Code)
	}
	tampered := f
	tampered.Body = strings.Replace(f.Body, "Retry uploads", "Retry uploadz", 1)
	if w := post(h, tampered, "/hooks/forgejo/acme"); w.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: %d", w.Code)
	}
	// Another workspace's org.
	h.Dir.(*fakeDir).ws.Spec.Forge.Org = "other"
	if w := post(h, f, "/hooks/forgejo/acme"); w.Code != http.StatusBadRequest {
		t.Fatalf("foreign org: %d", w.Code)
	}
	h.Dir.(*fakeDir).ws.Spec.Forge.Org = "acme"
	// Events switched off.
	h.Dir.(*fakeDir).ws.Spec.ForgeNotifications = []v1alpha1.ForgeEvent{v1alpha1.ForgeCommented}
	post(h, f, "/hooks/forgejo/acme")
	if len(s.msgs) != 0 {
		t.Fatalf("disabled event sent: %+v", s.msgs)
	}
	if w := httptest.NewRecorder(); true {
		h.ServeHTTP(w, httptest.NewRequest("GET", "/hooks/forgejo/acme", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET: %d", w.Code)
		}
	}
}

func sign(body string) string {
	m := hmac.New(sha256.New, []byte(fixtureSecret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// ciPayload follows Forgejo 17's WorkflowRunPayload / ActionPayload
// (modules/structs/hook.go and action.go); no runner was available to capture one.
func ciPayload(action string, runEvent string) string {
	ep, _ := json.Marshal(map[string]any{"pull_request": map[string]any{
		"number": 2, "title": "Retry uploads", "user": map[string]string{"login": "athena"},
		"html_url": "http://localhost:3000/acme/api/pulls/2"}})
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"run": map[string]any{
			"id": 7, "title": "Retry uploads", "status": "failure", "event": runEvent, "event_payload": string(ep),
			"prettyref": "retry", "html_url": "http://localhost:3000/acme/api/actions/runs/7",
			"trigger_user": map[string]string{"login": "athena"},
			"repository":   map[string]any{"name": "api", "full_name": "acme/api", "owner": map[string]string{"login": "acme"}, "default_branch": "main"},
		},
	})
	return string(b)
}

func TestCIFailed(t *testing.T) {
	h, s, red := newHandler()
	body := ciPayload("run_completed", "pull_request")
	f := fixture{Event: "workflow_run_failure", EventType: "workflow_run_failure", Delivery: "d-ci", Signature: sign(body), Body: body}
	if w := post(h, f, "/hooks/forgejo/acme"); w.Code != http.StatusNoContent {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if len(s.msgs) != 1 || s.msgs[0].to != "athena@acme" || !strings.HasPrefix(s.msgs[0].body, `CI failed on your pull request acme/api#2 "Retry uploads"`) {
		t.Fatalf("sent %+v", s.msgs)
	}
	if len(*red) != 1 || (*red)[0].Kind != v1alpha1.EventCIRed || (*red)[0].Room != "athena" {
		t.Fatalf("ciRed %+v", *red)
	}
	// The same run as action_run_failure doesn't notify twice.
	body2 := ciPayload("failure", "pull_request")
	f2 := fixture{Event: "action_run_failure", EventType: "action_run_failure", Delivery: "d-ci2", Signature: sign(body2), Body: body2}
	post(h, f2, "/hooks/forgejo/acme")
	if len(s.msgs) != 1 || len(*red) != 1 {
		t.Fatalf("duplicate CI notice: %d %d", len(s.msgs), len(*red))
	}
}

func TestMentions(t *testing.T) {
	got := Mentions("hi @Athena, see `@notme` and mail a@b.com\n```\n@hidden\n```\n(@hermes). @metis.")
	want := []string{"athena", "hermes", "metis"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Mentions = %v, want %v", got, want)
	}
}

func TestExcerptAndVerify(t *testing.T) {
	if r := []rune(excerpt(strings.Repeat("é", 600))); len(r) != ExcerptLen {
		t.Fatalf("excerpt length %d", len(r))
	}
	if !Verify([]byte("k"), []byte("b"), "sha256="+hex.EncodeToString(hmacSum("k", "b"))) {
		t.Fatal("sha256= prefix not accepted")
	}
	if Verify([]byte("k"), []byte("b"), "") || Verify([]byte("k"), []byte("b"), "zz") {
		t.Fatal("empty or malformed signature accepted")
	}
	_ = bytes.MinRead
}

func hmacSum(k, b string) []byte {
	m := hmac.New(sha256.New, []byte(k))
	m.Write([]byte(b))
	return m.Sum(nil)
}
