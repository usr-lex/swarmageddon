// Package forgehook receives Forgejo's org webhooks at
// /hooks/forgejo/<workspace> (docs/design.md §9.7, §10.3).
//
// Each delivery is verified with HMAC-SHA256 (X-Forgejo-Signature, hex) under
// the workspace's secret (Secret forge-webhook-<workspace>, key secret). Forge
// activity that concerns an agent is sent to it through messaging as
// forge@<workspace>; failed CI runs also raise the people-facing ciRed
// notification.
//
// Event names and payloads were checked against a real Forgejo 17.0.0:
// X-Forgejo-Event-Type is the specific type (issue_assign,
// pull_request_review_request, pull_request_comment,
// pull_request_review_approved, ...). There is no "workflow_run" event in
// Forgejo; CI failures come as workflow_run_failure or action_run_failure,
// both carrying a "run" object.
package forgehook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// MaxBody bounds a delivery's size.
const MaxBody = 4 << 20

// ExcerptLen is the longest excerpt quoted in a notice.
const ExcerptLen = 500

// Permissions tells whether a forge user can read a repository.
type Permissions interface {
	CanRead(ctx context.Context, owner, repo, user string) (bool, error)
}

// Sender sends a message as a system or person address.
type Sender interface {
	SendAs(ctx context.Context, as, id, to, body, replyTo string) (*messagingv1.SendResponse, error)
}

// Directory knows the workspaces and their rooms.
type Directory interface {
	Workspace(ctx context.Context, name string) (*v1alpha1.Workspace, error)
	// IsRoom reports whether a forge user is a room (agent) of the workspace.
	IsRoom(ctx context.Context, workspace, name string) (bool, error)
	// Secret returns the workspace's webhook secret.
	Secret(ctx context.Context, workspace string) ([]byte, error)
}

// Handler serves POST /hooks/forgejo/<workspace>.
type Handler struct {
	Dir   Directory
	Perms Permissions
	Send  Sender
	// CIRed raises the people-facing notification; optional.
	CIRed func(ctx context.Context, ev notify.Event)
	// Record stores observatory events (§8.7); optional.
	Record func(ctx context.Context, evs ...store.Activity)
	Audit func(ctx context.Context, ev store.AuditEvent)
	Log   *slog.Logger

	mu      sync.Mutex
	seenRun map[string]time.Time // workspace/run ID, so the two CI event kinds don't both notify
}

// Notice is one message to one agent.
type Notice struct {
	To    string // room name
	Event v1alpha1.ForgeEvent
	Body  string
	ID    string
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// ServeHTTP verifies and handles one delivery.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ws := strings.Trim(strings.TrimPrefix(r.URL.Path, "/hooks/forgejo/"), "/")
	if ws == "" || strings.Contains(ws, "/") {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil || len(body) > MaxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	ctx := r.Context()
	secret, err := h.Dir.Secret(ctx, ws)
	if err != nil || len(secret) == 0 {
		h.log().Warn("forge hook: no secret for workspace", "workspace", ws, "err", err)
		http.NotFound(w, r)
		return
	}
	if !Verify(secret, body, r.Header.Get("X-Forgejo-Signature")) {
		h.audit(ctx, store.AuditEvent{Actor: "forge", Action: "forgehook.refuse", Workspace: ws, Result: "bad signature",
			SourceIP: r.RemoteAddr})
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	d := Delivery{
		Workspace: ws,
		Event:     r.Header.Get("X-Forgejo-Event"),
		EventType: r.Header.Get("X-Forgejo-Event-Type"),
		ID:        r.Header.Get("X-Forgejo-Delivery"),
		Body:      body,
	}
	if err := h.Handle(ctx, d); err != nil {
		h.log().Warn("forge hook", "workspace", ws, "event", d.EventType, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Verify checks a hex HMAC-SHA256 signature of body.
func Verify(secret, body []byte, sig string) bool {
	want, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil || len(want) != sha256.Size {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}

// Delivery is one verified webhook delivery.
type Delivery struct {
	Workspace, Event, EventType, ID string
	Body                            []byte
}

// Handle turns a delivery into notices and sends them.
func (h *Handler) Handle(ctx context.Context, d Delivery) error {
	var p payload
	if err := json.Unmarshal(d.Body, &p); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	wsObj, err := h.Dir.Workspace(ctx, d.Workspace)
	if err != nil {
		return fmt.Errorf("workspace %s: %w", d.Workspace, err)
	}
	repo := p.repo()
	if repo == nil || !strings.EqualFold(repo.Owner.Login, wsObj.Spec.Forge.Org) {
		return errors.New("delivery is not about this workspace's org")
	}
	if h.Record != nil {
		h.Record(ctx, Activities(d, &p, time.Now().UTC())...)
	}
	if isCI(d) {
		if !h.firstForRun(d.Workspace, p.Run) {
			return nil
		}
		h.ciRed(ctx, d, &p, wsObj)
	}
	notices := Notices(d, &p)
	enabled := wsObj.Spec.ForgeNotifications
	for _, n := range notices {
		if len(enabled) > 0 && !slices.Contains(enabled, n.Event) {
			continue
		}
		ok, err := h.Dir.IsRoom(ctx, d.Workspace, n.To)
		if err != nil || !ok {
			continue
		}
		readable, err := h.Perms.CanRead(ctx, repo.Owner.Login, repo.Name, n.To)
		if err != nil {
			h.log().Warn("forge hook: permission check failed; not notifying", "user", n.To, "repo", repo.FullName, "err", err)
			continue
		}
		if !readable {
			continue
		}
		from := "forge@" + d.Workspace
		to := n.To + "@" + d.Workspace
		_, err = h.Send.SendAs(ctx, from, n.ID, to, n.Body, "")
		res := "ok"
		if err != nil {
			res = err.Error()
			h.log().Warn("forge hook: send failed", "to", to, "err", err)
		}
		h.audit(ctx, store.AuditEvent{Actor: from, Action: "forge.notify", Object: "room/" + n.To, Workspace: d.Workspace,
			Result: res, Detail: fmt.Sprintf(`{"event":%q,"repo":%q,"delivery":%q}`, n.Event, repo.FullName, d.ID)})
	}
	return nil
}

func (h *Handler) audit(ctx context.Context, ev store.AuditEvent) {
	if h.Audit != nil {
		h.Audit(ctx, ev)
	}
}

func (h *Handler) firstForRun(ws string, run *actionRun) bool {
	if run == nil || run.ID == 0 {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seenRun == nil {
		h.seenRun = map[string]time.Time{}
	}
	now := time.Now()
	for k, t := range h.seenRun {
		if now.Sub(t) > time.Hour {
			delete(h.seenRun, k)
		}
	}
	k := fmt.Sprint(ws, "/", run.ID)
	if _, ok := h.seenRun[k]; ok {
		return false
	}
	h.seenRun[k] = now
	return true
}

func (h *Handler) ciRed(ctx context.Context, d Delivery, p *payload, ws *v1alpha1.Workspace) {
	if h.CIRed == nil || p.Run == nil {
		return
	}
	run := p.Run
	pr := run.pullRequest()
	repo := p.repo()
	onDefault := repo != nil && repo.DefaultBranch != "" && (run.PrettyRef == repo.DefaultBranch || run.PrettyRef == "refs/heads/"+repo.DefaultBranch)
	if pr == nil && !onDefault {
		return
	}
	room := ""
	where := "on " + run.PrettyRef
	if pr != nil {
		room = strings.ToLower(pr.User.Login)
		where = fmt.Sprintf("on pull request %s#%d", repo.FullName, pr.Number)
	} else {
		where = fmt.Sprintf("on %s %s", repo.FullName, run.PrettyRef)
	}
	h.CIRed(ctx, notify.Event{Kind: v1alpha1.EventCIRed, Workspace: d.Workspace, Room: room, Fingerprint: fmt.Sprint(run.ID),
		Title: "CI failed " + where, Body: strings.TrimSpace(run.Title + " " + run.HTMLURL), URL: run.HTMLURL})
}

func isCI(d Delivery) bool {
	for _, e := range []string{d.Event, d.EventType} {
		if e == "workflow_run_failure" || e == "action_run_failure" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- payloads

type user struct {
	Login string `json:"login"`
}

type repository struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         user   `json:"owner"`
	DefaultBranch string `json:"default_branch"`
}

type issue struct {
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	User        user            `json:"user"`
	Assignees   []user          `json:"assignees"`
	HTMLURL     string          `json:"html_url"`
	PullRequest json.RawMessage `json:"pull_request"`
	Merged      bool            `json:"merged"`
	MergedBy    *user           `json:"merged_by"`
}

type commit struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	URL       string    `json:"url"`
	Timestamp time.Time `json:"timestamp"`
	Author    struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"author"`
}

type comment struct {
	Body    string `json:"body"`
	User    user   `json:"user"`
	HTMLURL string `json:"html_url"`
}

type review struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type actionRun struct {
	ID           int64       `json:"id"`
	Title        string      `json:"title"`
	Repo         *repository `json:"repository"`
	TriggerUser  *user       `json:"trigger_user"`
	PrettyRef    string      `json:"prettyref"`
	Event        string      `json:"event"`
	EventPayload string      `json:"event_payload"`
	Status       string      `json:"status"`
	HTMLURL      string      `json:"html_url"`
}

// pullRequest returns the PR a run was triggered by, from its event payload.
func (r *actionRun) pullRequest() *issue {
	if !strings.HasPrefix(r.Event, "pull_request") || r.EventPayload == "" {
		return nil
	}
	var ep struct {
		PullRequest *issue `json:"pull_request"`
	}
	if json.Unmarshal([]byte(r.EventPayload), &ep) != nil || ep.PullRequest == nil || ep.PullRequest.Number == 0 {
		return nil
	}
	return ep.PullRequest
}

type payload struct {
	Action            string      `json:"action"`
	Issue             *issue      `json:"issue"`
	PullRequest       *issue      `json:"pull_request"`
	Comment           *comment    `json:"comment"`
	Review            *review     `json:"review"`
	RequestedReviewer *user       `json:"requested_reviewer"`
	Repository        *repository `json:"repository"`
	Sender            *user       `json:"sender"`
	Run               *actionRun  `json:"run"`
	Commits           []commit    `json:"commits"`
	Pusher            *user       `json:"pusher"`
	Ref               string      `json:"ref"`
}

func (p *payload) repo() *repository {
	if p.Repository != nil {
		return p.Repository
	}
	if p.Run != nil {
		return p.Run.Repo
	}
	return nil
}

// item is the issue or PR the delivery is about.
func (p *payload) item() *issue {
	if p.PullRequest != nil {
		return p.PullRequest
	}
	return p.Issue
}

// ---------------------------------------------------------------- notices

var mentionRE = regexp.MustCompile(`(?:^|[^\w@/.\-])@([A-Za-z0-9][A-Za-z0-9_.\-]*)`)

// Mentions returns the lower-cased users @mentioned in text, outside code.
func Mentions(text string) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		line = stripInlineCode(line)
		for _, m := range mentionRE.FindAllStringSubmatch(line, -1) {
			name := strings.ToLower(strings.TrimRight(m[1], ".-_"))
			if name != "" && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

func stripInlineCode(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '`' {
			in = !in
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// priority: one notice per agent per delivery; the most specific wins.
var priority = map[v1alpha1.ForgeEvent]int{
	v1alpha1.ForgeReviewRequested: 6,
	v1alpha1.ForgeAssigned:        5,
	v1alpha1.ForgeReviewSubmitted: 4,
	v1alpha1.ForgeCIFailed:        3,
	v1alpha1.ForgeMentioned:       2,
	v1alpha1.ForgeCommented:       1,
}

// Notices computes who hears about a delivery, before the workspace,
// room and permission filters. The actor never hears about their own action.
func Notices(d Delivery, p *payload) []Notice {
	repo := p.repo()
	if repo == nil {
		return nil
	}
	actor := ""
	if p.Sender != nil {
		actor = strings.ToLower(p.Sender.Login)
	}
	type cand struct {
		ev      v1alpha1.ForgeEvent
		line    string
		excerpt string
		url     string
	}
	got := map[string]cand{}
	add := func(who string, c cand) {
		who = strings.ToLower(who)
		if who == "" || who == actor {
			return
		}
		if old, ok := got[who]; ok && priority[old.ev] >= priority[c.ev] {
			return
		}
		got[who] = c
	}
	it := p.item()
	ref := func() string {
		if it == nil {
			return repo.FullName
		}
		return fmt.Sprintf("%s#%d %q", repo.FullName, it.Number, it.Title)
	}
	kind := d.EventType
	if kind == "" {
		kind = d.Event
	}
	switch {
	case isCI(d):
		if p.Run == nil || p.Run.Status != "failure" && p.Run.Status != "" {
			break
		}
		pr := p.Run.pullRequest()
		if pr == nil {
			break
		}
		actor = "" // the run is the actor; the author still hears about it
		add(pr.User.Login, cand{ev: v1alpha1.ForgeCIFailed,
			line: fmt.Sprintf("CI failed on your pull request %s#%d %q", repo.FullName, pr.Number, pr.Title),
			url:  p.Run.HTMLURL, excerpt: p.Run.Title})
	case (kind == "issue_assign" || kind == "pull_request_assign") && p.Action == "assigned" && it != nil:
		for _, a := range it.Assignees {
			add(a.Login, cand{ev: v1alpha1.ForgeAssigned, line: fmt.Sprintf("%s assigned you %s", actor, ref()), url: it.HTMLURL, excerpt: it.Body})
		}
	case kind == "pull_request_review_request" && p.Action == "review_requested" && it != nil:
		if p.RequestedReviewer != nil {
			add(p.RequestedReviewer.Login, cand{ev: v1alpha1.ForgeReviewRequested,
				line: fmt.Sprintf("%s requested your review on %s", actor, ref()), url: it.HTMLURL, excerpt: it.Body})
		}
	case (kind == "issues" || kind == "pull_request") && p.Action == "opened" && it != nil:
		for _, m := range Mentions(it.Body) {
			add(m, cand{ev: v1alpha1.ForgeMentioned, line: fmt.Sprintf("%s mentioned you in %s", actor, ref()), url: it.HTMLURL, excerpt: it.Body})
		}
	case (kind == "issue_comment" || kind == "pull_request_comment") && p.Action == "created" && it != nil && p.Comment != nil:
		c := cand{ev: v1alpha1.ForgeCommented, line: fmt.Sprintf("%s commented on %s", actor, ref()), url: p.Comment.HTMLURL, excerpt: p.Comment.Body}
		add(it.User.Login, c)
		for _, a := range it.Assignees {
			add(a.Login, c)
		}
		for _, m := range Mentions(p.Comment.Body) {
			add(m, cand{ev: v1alpha1.ForgeMentioned, line: fmt.Sprintf("%s mentioned you in %s", actor, ref()), url: p.Comment.HTMLURL, excerpt: p.Comment.Body})
		}
	case strings.HasPrefix(kind, "pull_request_review_") && kind != "pull_request_review_request" && p.Review != nil && it != nil:
		verb := "commented on"
		switch kind {
		case "pull_request_review_approved":
			verb = "approved"
		case "pull_request_review_rejected":
			verb = "requested changes on"
		}
		add(it.User.Login, cand{ev: v1alpha1.ForgeReviewSubmitted,
			line: fmt.Sprintf("%s %s your pull request %s", actor, verb, ref()), url: it.HTMLURL, excerpt: p.Review.Content})
		for _, m := range Mentions(p.Review.Content) {
			add(m, cand{ev: v1alpha1.ForgeMentioned, line: fmt.Sprintf("%s mentioned you in a review of %s", actor, ref()), url: it.HTMLURL, excerpt: p.Review.Content})
		}
	}
	var out []Notice
	for who, c := range got {
		body := c.line
		if c.url != "" {
			body += "\n" + c.url
		}
		if ex := excerpt(c.excerpt); ex != "" {
			body += "\n\n" + ex
		}
		out = append(out, Notice{To: who, Event: c.ev, Body: body, ID: MessageID(d.ID, who)})
	}
	slices.SortFunc(out, func(a, b Notice) int { return strings.Compare(a.To, b.To) })
	return out
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > ExcerptLen {
		return string(r[:ExcerptLen-1]) + "…"
	}
	return s
}

// MessageID derives a stable ULID-shaped ID from the delivery and the
// recipient, so Forgejo's retries deduplicate in messaging.
func MessageID(delivery, recipient string) string {
	sum := sha256.Sum256([]byte("forge/" + delivery + "/" + recipient))
	var id ulid.ULID
	copy(id[:], sum[:16])
	id[0] &= 0x7f // keep the timestamp part in range
	return id.String()
}

// ---------------------------------------------------------------- forge permissions

// ForgeReader checks repository permissions with a read-only Forgejo token.
type ForgeReader struct {
	BaseURL string // e.g. http://forgejo.swarmageddon-platform.svc:3000
	Token   func(ctx context.Context) (string, error)
	HTTP    *http.Client
}

// CanRead implements Permissions with
// GET /api/v1/repos/{owner}/{repo}/collaborators/{user}/permission, which
// counts team access (verified on Forgejo 17).
func (f *ForgeReader) CanRead(ctx context.Context, owner, repo, u string) (bool, error) {
	tok, err := f.Token(ctx)
	if err != nil {
		return false, err
	}
	endpoint := fmt.Sprintf("%s/api/v1/repos/%s/%s/collaborators/%s/permission", strings.TrimRight(f.BaseURL, "/"),
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(u))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "token "+tok)
	c := f.HTTP
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("forge permission check: %s", resp.Status)
	}
	var out struct {
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	switch out.Permission {
	case "read", "write", "admin", "owner":
		return true, nil
	}
	return false, nil
}
