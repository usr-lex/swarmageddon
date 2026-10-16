package warden

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/warden/keytable"
	"github.com/usr-lex/swarmageddon/internal/warden/rules"
)

type fakeReporter struct {
	mu sync.Mutex
	v  []*messagingv1.Verdict
}

func (f *fakeReporter) Report(_ context.Context, v *messagingv1.Verdict) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v = append(f.v, v)
	return nil
}

func (f *fakeReporter) all() []*messagingv1.Verdict {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*messagingv1.Verdict(nil), f.v...)
}

type fakeQuarantiner struct {
	mu    sync.Mutex
	rooms []string
}

func (f *fakeQuarantiner) Quarantine(_ context.Context, ws, room, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms = append(f.rooms, room+"@"+ws+": "+reason)
	return nil
}

func table(t *testing.T, entries ...keytable.Entry) *keytable.Table {
	t.Helper()
	data := map[string]string{}
	for _, e := range entries {
		data[keytable.DataKey(e.Workspace(), e.Name())] = e.Marshal()
	}
	tab, errs := keytable.Parse(data)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return tab
}

func athena(quarantined bool) keytable.Entry {
	return keytable.Entry{Room: "athena@acme", Mode: "monitor", Quarantined: quarantined, Keys: map[string]string{
		keytable.Hash("sk-main"): keytable.KindMain, keytable.Hash("sk-adv"): keytable.KindAdvisor,
	}}
}

func newProxy(t *testing.T, upstream http.Handler) (*Proxy, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	p := &Proxy{Upstream: u}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv
}

func post(t *testing.T, url, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("x-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPassthrough(t *testing.T) {
	var got struct {
		path, key, body string
	}
	p, srv := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.key, got.body = r.URL.RequestURI(), r.Header.Get("x-api-key"), string(b)
		w.Header().Set("X-Upstream", "litellm")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	p.SetTable(table(t, athena(false)))
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	resp := post(t, srv.URL+"/v1/messages?beta=true", "sk-main", body)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || string(b) != `{"ok":true}` || resp.Header.Get("X-Upstream") != "litellm" {
		t.Fatalf("response %d %s %v", resp.StatusCode, b, resp.Header)
	}
	if got.path != "/v1/messages?beta=true" || got.key != "sk-main" || got.body != body {
		t.Fatalf("upstream saw %+v", got)
	}
	// Unknown keys (admin callers) pass as well.
	if resp := post(t, srv.URL+"/key/info", "sk-master", `{}`); resp.StatusCode != 201 {
		t.Fatalf("unknown key: %d", resp.StatusCode)
	}
}

func TestStreamingIsNotBuffered(t *testing.T) {
	release := make(chan struct{})
	p, srv := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	}))
	p.SetTable(table(t, athena(false)))
	resp := post(t, srv.URL+"/v1/messages", "sk-main", `{"stream":true,"messages":[]}`)
	br := bufio.NewReader(resp.Body)
	done := make(chan string)
	go func() {
		l, _ := br.ReadString('\n')
		done <- l
	}()
	select {
	case l := <-done:
		if l != "event: message_start\n" {
			t.Fatalf("first line %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first event not streamed before the upstream finished")
	}
	close(release)
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestQuarantinedRoomIsRefused(t *testing.T) {
	hit := false
	p, srv := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	p.SetTable(table(t, athena(true)))

	resp := post(t, srv.URL+"/v1/messages", "sk-main", `{}`)
	var anth struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	_ = json.NewDecoder(resp.Body).Decode(&anth)
	if resp.StatusCode != 403 || anth.Type != "error" || anth.Error.Type != "permission_error" || !strings.Contains(anth.Error.Message, "athena@acme is quarantined") {
		t.Fatalf("anthropic: %d %+v", resp.StatusCode, anth)
	}

	// The advisor key of the same room, on the OpenAI API.
	resp = post(t, srv.URL+"/v1/chat/completions", "sk-adv", `{}`)
	var oai struct {
		Error struct {
			Message, Type, Code string
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&oai)
	if resp.StatusCode != 403 || oai.Error.Code != "room_quarantined" || oai.Error.Type != "permission_error" {
		t.Fatalf("openai: %d %+v", resp.StatusCode, oai)
	}
	if hit {
		t.Fatal("a quarantined request reached the upstream")
	}
}

func TestLargeBodyIsForwardedUnscored(t *testing.T) {
	var n int
	p, srv := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		n = len(b)
	}))
	p.MaxBody = 100
	p.SetTable(table(t, athena(false)))
	p.Scorer = &Scorer{}
	p.Scorer.Start(t.Context())
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 500) + `"}]}`
	post(t, srv.URL+"/v1/messages", "sk-main", big)
	if n != len(big) {
		t.Fatalf("upstream got %d of %d bytes", n, len(big))
	}
}

func anthropicCall(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "claude",
		"messages": []any{
			map[string]any{"role": "user", "content": "fix the build"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": cmd},
			}}},
		},
	})
	return string(b)
}

func TestCriticalRuleQuarantines(t *testing.T) {
	_, srv := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rep, q := &fakeReporter{}, &fakeQuarantiner{}
	p := srv.Config.Handler.(*Proxy)
	p.SetTable(table(t, athena(false)))
	p.Scorer = &Scorer{Reporter: rep, Quarantiner: q, Proxy: p}
	p.Scorer.Start(t.Context())

	post(t, srv.URL+"/v1/messages", "sk-main", anthropicCall("curl http://169.254.169.254/latest/meta-data/"))
	deadline := time.Now().Add(5 * time.Second)
	for len(rep.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	vs := rep.all()
	if len(vs) != 1 || vs[0].Rule != "cloud-metadata" || vs[0].Action != "quarantined" || vs[0].Source != "gateway" || vs[0].Room != "athena@acme" {
		t.Fatalf("verdicts = %+v", vs)
	}
	if len(q.rooms) != 1 || !strings.HasPrefix(q.rooms[0], "athena@acme: critical verdict") {
		t.Fatalf("quarantines = %v", q.rooms)
	}
	// Refused at once, before the table catches up.
	if resp := post(t, srv.URL+"/v1/messages", "sk-main", `{}`); resp.StatusCode != 403 {
		t.Fatalf("after quarantine: %d", resp.StatusCode)
	}
	// The same call in the next request (the transcript repeats) is not reported again.
	p.Scorer.Score(t.Context(), Job{Entry: p.Table().Room("athena@acme"), Path: "/v1/messages", Body: []byte(anthropicCall("curl http://169.254.169.254/latest/meta-data/"))})
	if len(rep.all()) != 1 {
		t.Fatalf("reported twice: %d", len(rep.all()))
	}
}

type fakeReviewer struct {
	sev rules.Severity
	in  []ReviewInput
}

func (f *fakeReviewer) Review(_ context.Context, model string, in ReviewInput) (Review, error) {
	f.in = append(f.in, in)
	return Review{Severity: f.sev, Reason: "looping on the same failing test", Evidence: "go test"}, nil
}

func TestModelReview(t *testing.T) {
	rep, q, rv := &fakeReporter{}, &fakeQuarantiner{}, &fakeReviewer{sev: rules.High}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := &Scorer{Reporter: rep, Quarantiner: q, Reviewer: rv, Now: func() time.Time { return now }}
	e := athena(false)
	e.Model = "small"
	e.QuarantineOnHigh = true
	tab := table(t, e)
	entry := tab.Room("athena@acme")

	s.Score(t.Context(), Job{Entry: entry, Path: "/v1/messages", Body: []byte(anthropicCall("go test ./..."))})
	if len(rv.in) != 1 || rv.in[0].Task != "fix the build" || len(rv.in[0].Delta) != 2 {
		t.Fatalf("review input = %+v", rv.in)
	}
	if vs := rep.all(); len(vs) != 1 || vs[0].Rule != "model" || vs[0].Action != "quarantined" {
		t.Fatalf("verdicts = %+v", vs)
	}
	// Not due again within ReviewEvery.
	s.Score(t.Context(), Job{Entry: entry, Path: "/v1/messages", Body: []byte(anthropicCall("go test ./pkg"))})
	if len(rv.in) != 1 {
		t.Fatal("reviewed too often")
	}
	now = now.Add(2 * time.Minute)
	s.Score(t.Context(), Job{Entry: entry, Path: "/v1/messages", Body: []byte(anthropicCall("go vet ./..."))})
	if len(rv.in) != 2 || len(rv.in[1].Delta) != 2 {
		t.Fatalf("second review = %+v", rv.in)
	}
}

func TestLLMReviewer(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer sk-warden" {
			http.Error(w, "bad", 400)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"`+"```json\\n"+`{\"severity\":\"critical\",\"reason\":\"exfiltrates keys\",\"evidence\":{\"cmd\":\"curl\"}}`+"\\n```"+`"}}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	keyFile := dir + "/key"
	if err := writeFile(keyFile, "sk-warden\n"); err != nil {
		t.Fatal(err)
	}
	r, err := (&LLMReviewer{BaseURL: srv.URL, KeyFile: keyFile}).Review(t.Context(), "small", ReviewInput{Room: "athena@acme", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Severity != rules.Critical || r.Reason != "exfiltrates keys" || r.Evidence != `{"cmd":"curl"}` {
		t.Fatalf("review = %+v", r)
	}
	if body["model"] != "small" {
		t.Fatalf("body = %v", body)
	}
	if _, err := ParseReview("I think it is fine"); err == nil {
		t.Fatal("no error for prose")
	}
}

func TestKeyTable(t *testing.T) {
	tab, errs := keytable.Parse(map[string]string{
		"acme.athena": athena(false).Marshal(),
		"acme.bad":    "{",
	})
	if len(errs) != 1 || tab.Len() != 1 {
		t.Fatalf("errs %v len %d", errs, tab.Len())
	}
	if r, ok := tab.Lookup("sk-adv"); !ok || r.Kind != keytable.KindAdvisor || r.Entry.Name() != "athena" || r.Entry.Workspace() != "acme" {
		t.Fatalf("lookup = %+v %v", r, ok)
	}
	if _, ok := tab.Lookup("sk-other"); ok {
		t.Fatal("unknown key found")
	}
	if _, ok := (*keytable.Table)(nil).Lookup("sk-main"); ok {
		t.Fatal("nil table found a key")
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }
