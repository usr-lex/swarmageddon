// Package notify tells people about events that need them (docs/design.md
// §10.3): browser push (VAPID), ntfy, Slack and Discord webhooks, and email
// over SMTP. Repeats are collapsed: one notification per room and event
// until the condition changes (a new fingerprint) or clears.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// Event is something people may be told about.
type Event struct {
	Kind      v1alpha1.NotificationEvent `json:"kind"`
	Workspace string                     `json:"workspace"`
	Room      string                     `json:"room,omitempty"`
	Title     string                     `json:"title"`
	Body      string                     `json:"body"`
	// URL opens the relevant page of the control center (relative path).
	URL string `json:"url,omitempty"`
	// Fingerprint identifies this occurrence; the same fingerprint for the
	// same room and event is not sent again.
	Fingerprint string `json:"-"`
}

// Dedup collapses repeats per workspace, room and event.
type Dedup struct {
	mu   sync.Mutex
	last map[string]string
}

func dedupKey(ws, room string, kind v1alpha1.NotificationEvent) string {
	return ws + "/" + room + "/" + string(kind)
}

// Should reports whether ev is new, and records it.
func (d *Dedup) Should(ev Event) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last = map[string]string{}
	}
	k := dedupKey(ev.Workspace, ev.Room, ev.Kind)
	if fp, ok := d.last[k]; ok && fp == ev.Fingerprint {
		return false
	}
	d.last[k] = ev.Fingerprint
	return true
}

// Clear forgets an event, so the next occurrence notifies again.
func (d *Dedup) Clear(ws, room string, kind v1alpha1.NotificationEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.last, dedupKey(ws, room, kind))
}

// Recipient is a person to push to, with the channels they chose for the event.
type Recipient struct {
	UserID   string
	Channels []string
	Push     []store.PushSubscription
}

// Directory answers who wants what.
type Directory interface {
	// Settings returns the workspace's notification settings.
	Settings(ctx context.Context, workspace string) (v1alpha1.NotificationSettings, error)
	// Recipients returns the people who can see the workspace, with their
	// channels for this event.
	Recipients(ctx context.Context, workspace string, kind v1alpha1.NotificationEvent) ([]Recipient, error)
	// Secret reads a channel's Secret in the system namespace.
	Secret(ctx context.Context, name string) (map[string][]byte, error)
	// DropPush removes a subscription the push service says is gone.
	DropPush(ctx context.Context, endpoint string)
}

// VAPID holds the web push keys (Secret vapid).
type VAPID struct {
	PublicKey, PrivateKey string
	Subscriber            string // mailto: or https: contact
}

// NtfySecretName is the system-namespace Secret with the ntfy publisher token.
const NtfySecretName = "ntfy-publisher"

// Dispatcher sends events.
type Dispatcher struct {
	Dedup   Dedup
	Dir     Directory
	VAPID   func() *VAPID
	NtfyURL string // base URL of ntfy
	// NtfySecret names the Secret (read through Dir.Secret) whose key token
	// is sent as Authorization: Bearer on publish; empty: NtfySecretName.
	NtfySecret string
	HTTP    *http.Client
	Log     *slog.Logger
	// Sent records each delivery attempt (tests, metrics).
	Sent func(channel string, ev Event, err error)
}

// Raise sends ev unless it repeats the last one for its room and event.
func (d *Dispatcher) Raise(ctx context.Context, ev Event) {
	if !d.Dedup.Should(ev) {
		return
	}
	settings, err := d.Dir.Settings(ctx, ev.Workspace)
	if err != nil {
		d.log().Warn("notify: workspace settings", "workspace", ev.Workspace, "err", err)
		return
	}
	if len(settings.Events) > 0 && !containsEvent(settings.Events, ev.Kind) {
		return
	}
	webPushOn := len(settings.Channels) == 0 // browser push is the default
	for _, ch := range settings.Channels {
		switch ch.Kind {
		case "webPush":
			webPushOn = true
		case "ntfy":
			d.record("ntfy", ev, d.ntfy(ctx, ch.Topic, ev))
		case "slack", "discord":
			d.record(string(ch.Kind), ev, d.webhook(ctx, string(ch.Kind), ch.SecretName, ev))
		case "email":
			d.record("email", ev, d.email(ctx, ch.SecretName, ev))
		}
	}
	if !webPushOn {
		return
	}
	recips, err := d.Dir.Recipients(ctx, ev.Workspace, ev.Kind)
	if err != nil {
		d.log().Warn("notify: recipients", "err", err)
		return
	}
	for _, r := range recips {
		if !contains(r.Channels, "webPush") {
			continue
		}
		for _, sub := range r.Push {
			d.record("webPush", ev, d.webPush(ctx, sub, ev))
		}
	}
}

// Clear forgets an event so its next occurrence notifies again.
func (d *Dispatcher) Clear(ws, room string, kind v1alpha1.NotificationEvent) {
	d.Dedup.Clear(ws, room, kind)
}

// SendTest sends a test notification on one channel to one person's push
// subscriptions or the workspace channels.
func (d *Dispatcher) SendTest(ctx context.Context, subs []store.PushSubscription) error {
	return d.PushTo(ctx, subs, Event{Kind: "test", Title: "swarmageddon", Body: "Notifications work.", URL: "/"})
}

// PushTo sends ev to the given browser subscriptions (a person's messages),
// without deduplication.
func (d *Dispatcher) PushTo(ctx context.Context, subs []store.PushSubscription, ev Event) error {
	var first error
	for _, s := range subs {
		if err := d.webPush(ctx, s, ev); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (d *Dispatcher) record(ch string, ev Event, err error) {
	if err != nil {
		d.log().Warn("notify: delivery failed", "channel", ch, "event", ev.Kind, "room", ev.Room, "err", err)
	}
	if d.Sent != nil {
		d.Sent(ch, ev, err)
	}
}

func (d *Dispatcher) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

func (d *Dispatcher) client() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (d *Dispatcher) webPush(ctx context.Context, sub store.PushSubscription, ev Event) error {
	if d.VAPID == nil {
		return fmt.Errorf("no VAPID keys")
	}
	v := d.VAPID()
	if v == nil {
		return fmt.Errorf("no VAPID keys")
	}
	payload, _ := json.Marshal(ev)
	resp, err := webpush.SendNotificationWithContext(ctx, payload,
		&webpush.Subscription{Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth}},
		&webpush.Options{HTTPClient: d.client(), Subscriber: v.Subscriber, VAPIDPublicKey: v.PublicKey,
			VAPIDPrivateKey: v.PrivateKey, TTL: 3600, Urgency: webpush.UrgencyHigh, Topic: topic(ev)})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		d.Dir.DropPush(ctx, sub.Endpoint)
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("push service: %s", resp.Status)
	}
	return nil
}

// topic collapses pending pushes of the same room and event (max 32 chars, URL-safe).
func topic(ev Event) string {
	t := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, ev.Workspace+"-"+ev.Room+"-"+string(ev.Kind))
	if len(t) > 32 {
		t = t[:32]
	}
	return t
}

func (d *Dispatcher) ntfy(ctx context.Context, topic string, ev Event) error {
	if d.NtfyURL == "" || topic == "" {
		return fmt.Errorf("ntfy is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.NtfyURL, "/")+"/"+topic, strings.NewReader(ev.Body))
	if err != nil {
		return err
	}
	// ntfy denies anonymous publishing (docs/conventions.md).
	name := d.NtfySecret
	if name == "" {
		name = NtfySecretName
	}
	sec, err := d.Dir.Secret(ctx, name)
	if err != nil {
		return fmt.Errorf("ntfy token (Secret %s): %w", name, err)
	}
	if tok := strings.TrimSpace(string(sec["token"])); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Title", ev.Title)
	req.Header.Set("Tags", string(ev.Kind))
	if ev.Kind == v1alpha1.EventApprovalWaiting || ev.Kind == v1alpha1.EventRoomCrashed {
		req.Header.Set("Priority", "high")
	}
	return do(d.client(), req)
}

func (d *Dispatcher) webhook(ctx context.Context, kind, secret string, ev Event) error {
	data, err := d.Dir.Secret(ctx, secret)
	if err != nil {
		return err
	}
	url := string(data["url"])
	if url == "" {
		return fmt.Errorf("secret %s has no url", secret)
	}
	text := fmt.Sprintf("*%s*\n%s", ev.Title, ev.Body)
	var body any = map[string]string{"text": text}
	if kind == "discord" {
		body = map[string]string{"content": fmt.Sprintf("**%s**\n%s", ev.Title, ev.Body)}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(d.client(), req)
}

func (d *Dispatcher) email(ctx context.Context, secret string, ev Event) error {
	data, err := d.Dir.Secret(ctx, secret)
	if err != nil {
		return err
	}
	host, port := string(data["host"]), string(data["port"])
	from, to := string(data["from"]), string(data["to"])
	if host == "" || from == "" || to == "" {
		return fmt.Errorf("secret %s needs host, from and to", secret)
	}
	if port == "" {
		port = "587"
	}
	var a smtp.Auth
	if u := string(data["username"]); u != "" {
		a = smtp.PlainAuth("", u, string(data["password"]), host)
	}
	msg := "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + sanitizeHeader(ev.Title) +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + ev.Body + "\r\n"
	// net/smtp uses STARTTLS when the server offers it and refuses PlainAuth without TLS.
	return smtp.SendMail(net.JoinHostPort(host, port), a, from, strings.Split(to, ","), []byte(msg))
}

func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

func do(c *http.Client, req *http.Request) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", req.URL.Host, resp.Status)
	}
	return nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsEvent(s []v1alpha1.NotificationEvent, v v1alpha1.NotificationEvent) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// GenerateVAPID returns new VAPID keys (URL-safe base64).
func GenerateVAPID() (private, public string, err error) {
	return webpush.GenerateVAPIDKeys()
}
