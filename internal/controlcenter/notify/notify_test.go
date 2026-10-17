package notify

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

type fakeDir struct {
	settings v1alpha1.NotificationSettings
	recips   []Recipient
	secrets  map[string]map[string][]byte
	dropped  []string
}

func (f *fakeDir) Settings(context.Context, string) (v1alpha1.NotificationSettings, error) {
	return f.settings, nil
}
func (f *fakeDir) Recipients(context.Context, string, v1alpha1.NotificationEvent) ([]Recipient, error) {
	return f.recips, nil
}
func (f *fakeDir) Secret(_ context.Context, n string) (map[string][]byte, error) {
	return f.secrets[n], nil
}
func (f *fakeDir) DropPush(_ context.Context, e string) { f.dropped = append(f.dropped, e) }

type hits struct {
	mu   sync.Mutex
	got  []string
	code int
}

func (h *hits) server(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.got = append(h.got, r.URL.Path+" "+r.Header.Get("Title")+" "+string(b))
		code := h.code
		h.mu.Unlock()
		if code == 0 {
			code = 201
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s
}

func (h *hits) n() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.got)
}

func pushSub(t *testing.T, endpoint string) store.PushSubscription {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	return store.PushSubscription{Endpoint: endpoint, P256dh: base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()),
		Auth: base64.RawURLEncoding.EncodeToString(auth)}
}

func TestDispatch(t *testing.T) {
	ntfy, slack, discord, push := &hits{}, &hits{}, &hits{}, &hits{}
	ns, ss, ds, ps := ntfy.server(t), slack.server(t), discord.server(t), push.server(t)
	priv, pub, err := GenerateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	dir := &fakeDir{
		settings: v1alpha1.NotificationSettings{
			Channels: []v1alpha1.NotificationChannel{{Kind: "webPush"}, {Kind: "ntfy", Topic: "acme"},
				{Kind: "slack", SecretName: "notify-acme-slack"}, {Kind: "discord", SecretName: "notify-acme-discord"}},
			Events: []v1alpha1.NotificationEvent{v1alpha1.EventApprovalWaiting, v1alpha1.EventDiskLow},
		},
		recips: []Recipient{
			{UserID: "u1", Channels: []string{"webPush"}, Push: []store.PushSubscription{pushSub(t, ps.URL+"/sub1")}},
			{UserID: "u2", Channels: nil, Push: []store.PushSubscription{pushSub(t, ps.URL+"/sub2")}}, // opted out
		},
		secrets: map[string]map[string][]byte{
			"notify-acme-slack":   {"url": []byte(ss.URL + "/hook")},
			"notify-acme-discord": {"url": []byte(ds.URL + "/hook")},
		},
	}
	d := &Dispatcher{Dir: dir, NtfyURL: ns.URL, VAPID: func() *VAPID { return &VAPID{PublicKey: pub, PrivateKey: priv, Subscriber: "mailto:ops@example.com"} }}
	var errs []error
	d.Sent = func(ch string, ev Event, err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	ctx := context.Background()
	ev := Event{Kind: v1alpha1.EventApprovalWaiting, Workspace: "acme", Room: "athena", Title: "athena needs approval", Body: "Bash: rm -rf build", Fingerprint: "a1"}
	d.Raise(ctx, ev)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if ntfy.n() != 1 || slack.n() != 1 || discord.n() != 1 || push.n() != 1 {
		t.Fatalf("deliveries ntfy=%d slack=%d discord=%d push=%d", ntfy.n(), slack.n(), discord.n(), push.n())
	}
	if !strings.Contains(ntfy.got[0], "/acme athena needs approval Bash: rm -rf build") {
		t.Fatalf("ntfy request %q", ntfy.got[0])
	}
	if !strings.Contains(slack.got[0], `"text"`) || !strings.Contains(discord.got[0], `"content"`) {
		t.Fatalf("webhook bodies %q %q", slack.got[0], discord.got[0])
	}
	if !strings.HasPrefix(push.got[0], "/sub1") {
		t.Fatalf("push went to %q", push.got[0])
	}

	// Same room, event and fingerprint: collapsed.
	d.Raise(ctx, ev)
	if ntfy.n() != 1 || push.n() != 1 {
		t.Fatal("repeat was not collapsed")
	}
	// A new occurrence notifies again; so does one after Clear.
	ev.Fingerprint = "a2"
	d.Raise(ctx, ev)
	d.Clear("acme", "athena", v1alpha1.EventApprovalWaiting)
	d.Raise(ctx, ev)
	if ntfy.n() != 3 {
		t.Fatalf("ntfy deliveries %d, want 3", ntfy.n())
	}
	// Another room is independent.
	ev.Room = "hermes"
	d.Raise(ctx, ev)
	if ntfy.n() != 4 {
		t.Fatal("other room collapsed")
	}
	// Events the workspace didn't choose are dropped.
	d.Raise(ctx, Event{Kind: v1alpha1.EventCIRed, Workspace: "acme", Room: "athena", Fingerprint: "x"})
	if ntfy.n() != 4 {
		t.Fatal("unselected event sent")
	}
	// A gone subscription is dropped.
	push.code = 410
	d.Raise(ctx, Event{Kind: v1alpha1.EventDiskLow, Workspace: "acme", Room: "athena", Fingerprint: "low"})
	if len(dir.dropped) != 1 || !strings.HasSuffix(dir.dropped[0], "/sub1") {
		t.Fatalf("dropped %v", dir.dropped)
	}
}

type openWork bool

func (o openWork) HasOpenWork(context.Context, string, string) (bool, error) { return bool(o), nil }

func TestTracker(t *testing.T) {
	ntfy := &hits{}
	ns := ntfy.server(t)
	dir := &fakeDir{settings: v1alpha1.NotificationSettings{Channels: []v1alpha1.NotificationChannel{{Kind: "ntfy", Topic: "acme"}}}}
	d := &Dispatcher{Dir: dir, NtfyURL: ns.URL}
	now := time.Unix(1000, 0)
	tr := NewTracker(d, openWork(true))
	tr.Now = func() time.Time { return now }
	ctx := context.Background()

	vol := func(used int64) *messagingv1.RoomStatusReport {
		return &messagingv1.RoomStatusReport{Volumes: []*messagingv1.VolumeUsage{{Name: "home", UsedBytes: used, CapacityBytes: 100}}}
	}
	tr.Presence(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_WORKING, vol(85))
	tr.Presence(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_WORKING, vol(90))
	if ntfy.n() != 1 {
		t.Fatalf("diskLow deliveries %d, want 1", ntfy.n())
	}
	tr.Presence(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_WORKING, vol(50))
	tr.Presence(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_WORKING, vol(81))
	if ntfy.n() != 2 {
		t.Fatalf("diskLow after recovery: %d", ntfy.n())
	}

	// Crashes.
	tr.Presence(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_OFFLINE, &messagingv1.RoomStatusReport{CrashCount: 3})
	tr.Presence(ctx, "acme", "athena", messagingv1.Presence_PRESENCE_OFFLINE, &messagingv1.RoomStatusReport{CrashCount: 4})
	if ntfy.n() != 3 {
		t.Fatalf("roomCrashed deliveries: %d", ntfy.n())
	}
	tr.PodRestarts(ctx, "acme", "hermes", 0, "")
	tr.PodRestarts(ctx, "acme", "hermes", 0, "")
	tr.PodRestarts(ctx, "acme", "hermes", 1, "room: OOMKilled, exit code 137")
	if ntfy.n() != 4 {
		t.Fatalf("pod restart deliveries: %d", ntfy.n())
	}

	// Idle with open work after 10 minutes, once.
	tr.Presence(ctx, "acme", "metis", messagingv1.Presence_PRESENCE_IDLE, nil)
	now = now.Add(9 * time.Minute)
	tr.Tick(ctx)
	if ntfy.n() != 4 {
		t.Fatal("idle raised too early")
	}
	now = now.Add(2 * time.Minute)
	tr.Tick(ctx)
	tr.Tick(ctx)
	tr.Presence(ctx, "acme", "metis", messagingv1.Presence_PRESENCE_IDLE, nil)
	tr.Tick(ctx)
	if ntfy.n() != 5 {
		t.Fatalf("idle deliveries %d, want 5", ntfy.n())
	}
	// Working again, then idle again: a new occurrence.
	tr.Presence(ctx, "acme", "metis", messagingv1.Presence_PRESENCE_WORKING, nil)
	tr.Presence(ctx, "acme", "metis", messagingv1.Presence_PRESENCE_IDLE, nil)
	now = now.Add(11 * time.Minute)
	tr.Tick(ctx)
	if ntfy.n() != 6 {
		t.Fatalf("second idle: %d", ntfy.n())
	}
}
