package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
)

// wall is remote-labs' wall relay as two mediated links (design §4).
func wall() *directory.Workspace {
	return &directory.Workspace{
		Name:   "acme",
		Groups: []string{"clean", "dirty", "ops"},
		Messaging: v1alpha1.MessagingPolicy{
			DefaultLinks: v1alpha1.DefaultWithinGroup,
			Links: []v1alpha1.GroupLink{
				{From: "clean", To: "dirty", Mode: v1alpha1.LinkMediated, Senders: []string{"hermes", "proteus"}, Recipients: []string{"metis"}},
				{From: "dirty", To: "clean", Mode: v1alpha1.LinkMediated, Senders: []string{"hecate"}, Recipients: []string{"hermes", "proteus"}, RequirePrefix: "to {recipient}:"},
				{From: "ops", To: "clean", Mode: v1alpha1.LinkOpen, RequirePrefix: "[ops]"},
				{From: "clean", To: "ops", Mode: v1alpha1.LinkNone},
			},
			Limits: v1alpha1.MessageLimits{MaxBytes: 100},
		},
		Rooms: map[string]directory.Room{
			"hermes":  {Name: "hermes", Group: "clean"},
			"proteus": {Name: "proteus", Group: "clean"},
			"themis":  {Name: "themis", Group: "clean"},
			"metis":   {Name: "metis", Group: "dirty"},
			"hecate":  {Name: "hecate", Group: "dirty"},
			"argus":   {Name: "argus", Group: "dirty"},
			"atlas":   {Name: "atlas", Group: "ops"},
			"loner":   {Name: "loner"},
		},
	}
}

func people(name string) bool { return name == "alice" || name == "bob" }

func TestCheck(t *testing.T) {
	agent := func(name string) Party { p, _ := Lookup(wall(), people, name); return p }
	alice := Party{Name: "alice", Kind: Human}
	tests := []struct {
		name     string
		ws       func(*directory.Workspace)
		from     Party
		to, body string
		want     []string // recipients; nil means refused
		code     Code
		rule     string
		reason   string // substring of the refusal
		link     string
		mediated bool
	}{
		// wall-relay.test.mjs: "a contact reaches the dirty side, without a way to answer around the keeper"
		{name: "contact reaches the dirty side", from: agent("hermes"), to: "metis", body: "please run the #161 parity replay",
			want: []string{"metis"}, link: "clean->dirty", mediated: true},
		{name: "full address", from: agent("hermes"), to: "metis@acme", body: "x", want: []string{"metis"}, link: "clean->dirty", mediated: true},
		{name: "recipient cannot answer around the keeper", from: agent("metis"), to: "hermes", body: "done",
			code: Denied, rule: "mediated-sender", reason: "only hecate may message hermes"},
		// "a clean room that is not a contact is turned away"
		{name: "non-contact is turned away", from: agent("themis"), to: "metis", body: "can you check something",
			code: Denied, rule: "mediated-sender", reason: "only hermes and proteus may message metis"},
		{name: "contact may only reach the inbox", from: agent("hermes"), to: "hecate", body: "hi",
			code: Denied, rule: "mediated-recipient", reason: "may only message metis"},
		// "the keeper answers a named contact"
		{name: "keeper answers a named contact", from: agent("hecate"), to: "proteus", body: "to proteus: #161 parity replay passed",
			want: []string{"proteus"}, link: "dirty->clean", mediated: true},
		// "nothing leaves without a contact named, or from anyone but the keeper"
		{name: "keeper without the prefix", from: agent("hecate"), to: "proteus", body: "the replay passed",
			code: Invalid, rule: "prefix", reason: `must start with "to proteus:"`},
		{name: "keeper with the wrong recipient's prefix", from: agent("hecate"), to: "hermes", body: "to proteus: x",
			code: Invalid, rule: "prefix", reason: `"to hermes:"`},
		{name: "keeper to a non-contact", from: agent("hecate"), to: "themis", body: "to themis: the replay passed",
			code: Denied, rule: "mediated-recipient", reason: "hecate may only message hermes and proteus"},
		{name: "not the keeper", from: agent("argus"), to: "proteus", body: "to proteus: raw output",
			code: Denied, rule: "mediated-sender", reason: "only hecate may message proteus"},
		{name: "no group address across a mediated link", from: agent("hermes"), to: "@group:dirty", body: "all",
			code: Denied, rule: "mediated-group", reason: "name exactly one recipient: metis"},
		// within a group
		{name: "within group", from: agent("hermes"), to: "themis", body: "hi", want: []string{"themis"}},
		{name: "group fan-out within group", from: agent("hermes"), to: "@group:clean", body: "hi", want: []string{"proteus", "themis"}},
		{name: "within group closed", ws: func(w *directory.Workspace) { w.Messaging.DefaultLinks = v1alpha1.DefaultNone },
			from: agent("hermes"), to: "themis", body: "hi", code: Denied, rule: "within-group", reason: "rooms in clean can't message each other"},
		{name: "within group closed but self link open", ws: func(w *directory.Workspace) {
			w.Messaging.DefaultLinks = v1alpha1.DefaultNone
			w.Messaging.Links = append(w.Messaging.Links, v1alpha1.GroupLink{From: "clean", To: "clean", Mode: v1alpha1.LinkOpen})
		}, from: agent("hermes"), to: "themis", body: "hi", want: []string{"themis"}},
		{name: "default group", from: agent("loner"), to: "hermes", body: "hi", code: Denied, rule: "no-link",
			reason: "there is no link from (no group) to clean"},
		// open links and required prefixes
		{name: "open link with prefix", from: agent("atlas"), to: "themis", body: "[ops] deploy at 5", want: []string{"themis"}, link: "ops->clean"},
		{name: "open link without prefix", from: agent("atlas"), to: "themis", body: "deploy at 5", code: Invalid, rule: "prefix", reason: `"[ops]"`},
		{name: "open link group fan-out", from: agent("atlas"), to: "@group:clean", body: "[ops] hi", want: []string{"hermes", "proteus", "themis"}, link: "ops->clean"},
		{name: "closed link", from: agent("themis"), to: "atlas", body: "hi", code: Denied, rule: "link-none", reason: "the link clean->ops is closed"},
		{name: "no link", from: agent("metis"), to: "atlas", body: "hi", code: Denied, rule: "no-link", reason: "metis may message rooms in dirty and clean"},
		// people
		{name: "agent to person", from: agent("metis"), to: "alice", body: "question", want: []string{"alice"}},
		{name: "person to agent across groups", from: alice, to: "metis", body: "hi", want: []string{"metis"}},
		{name: "person to group", from: alice, to: "@group:dirty", body: "hi", want: []string{"argus", "hecate", "metis"}},
		{name: "person to person", from: alice, to: "bob@acme", body: "hi", want: []string{"bob"}},
		{name: "humans not reachable", ws: func(w *directory.Workspace) { f := false; w.Messaging.HumansReachable = &f },
			from: agent("metis"), to: "alice", body: "hi", code: Denied, rule: "humans"},
		// forge notices (§9.7)
		{name: "forge bypasses links", from: Party{Name: "forge", Kind: System}, to: "metis", body: "hermes requested your review", want: []string{"metis"}},
		{name: "forge to a person", from: Party{Name: "forge", Kind: System}, to: "alice", body: "x", code: Invalid, rule: "forge"},
		{name: "forge to a group", from: Party{Name: "forge", Kind: System}, to: "@group:clean", body: "x", code: Invalid, rule: "forge"},
		{name: "forge size limit", from: Party{Name: "forge", Kind: System}, to: "metis", body: strings.Repeat("x", 101), code: Invalid, rule: "size"},
		{name: "agents can't reply to forge", from: agent("hermes"), to: "forge", body: "thanks", code: Invalid, rule: "forge",
			reason: "forge notices are one-way; reply on the issue or PR"},
		{name: "people can't reply to forge", from: alice, to: "forge@acme", body: "thanks", code: Invalid, rule: "forge"},
		// status reports (§8.7)
		{name: "any agent reports to status", from: agent("metis"), to: "status", body: "shipped #42", want: []string{"status"}},
		{name: "ungrouped agent reports to status", from: agent("loner"), to: "status@acme", body: "x", want: []string{"status"}},
		{name: "status report size limit", from: agent("metis"), to: "status", body: strings.Repeat("x", 101), code: Invalid, rule: "size"},
		{name: "people don't report to status", from: alice, to: "status", body: "x", code: Denied, rule: "status"},
		{name: "status can't send", from: Party{Name: "status", Kind: System}, to: "metis", body: "x", code: Denied, rule: "system"},
		{name: "forge can't send to status", from: Party{Name: "forge", Kind: System}, to: "status", body: "x", code: Invalid, rule: "forge"},
		// addressing and limits
		{name: "unknown recipient", from: agent("hermes"), to: "zeus", body: "hi", code: NotFound, rule: "unknown-recipient"},
		{name: "unknown group", from: agent("hermes"), to: "@group:olympus", body: "hi", code: NotFound, rule: "unknown-group"},
		{name: "other workspace", from: agent("hermes"), to: "metis@other", body: "hi", code: Denied, rule: "workspace"},
		{name: "federation reserved", from: agent("hermes"), to: "metis@acme.east", body: "hi", code: Invalid, rule: "address"},
		{name: "bad address", from: agent("hermes"), to: "Metis!", body: "hi", code: Invalid, rule: "address"},
		{name: "self", from: agent("hermes"), to: "hermes", body: "hi", code: Invalid, rule: "self"},
		{name: "too big", from: agent("hermes"), to: "themis", body: strings.Repeat("x", 101), code: Invalid, rule: "size", reason: "at most 100"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := wall()
			if tc.ws != nil {
				tc.ws(ws)
			}
			route, r := Check(ws, people, tc.from, tc.to, tc.body)
			if tc.want == nil {
				if r == nil {
					t.Fatalf("allowed to %v; want refusal %s", route.Recipients, tc.rule)
				}
				if r.Code != tc.code || r.Rule != tc.rule || !strings.Contains(r.Reason, tc.reason) {
					t.Fatalf("refusal = %d %s %q; want %d %s containing %q", r.Code, r.Rule, r.Reason, tc.code, tc.rule, tc.reason)
				}
				return
			}
			if r != nil {
				t.Fatalf("refused: %s: %s", r.Rule, r.Reason)
			}
			var got []string
			for _, p := range route.Recipients {
				got = append(got, p.Name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("recipients = %v; want %v", got, tc.want)
			}
			if route.LinkTo(got[0]) != tc.link || route.Mediated != tc.mediated {
				t.Fatalf("link = %q mediated=%v; want %q %v", route.LinkTo(got[0]), route.Mediated, tc.link, tc.mediated)
			}
		})
	}
}

func TestAllowed(t *testing.T) {
	ws := wall()
	p := func(n string) Party { x, _ := Lookup(ws, people, n); return x }
	if ok, note := Allowed(ws, p("hecate"), p("proteus")); !ok || !strings.Contains(note, `"to proteus:"`) {
		t.Fatalf("hecate->proteus = %v %q", ok, note)
	}
	if ok, _ := Allowed(ws, p("argus"), p("proteus")); ok {
		t.Fatal("argus->proteus allowed")
	}
	if ok, note := Allowed(ws, p("hermes"), p("themis")); !ok || note != "" {
		t.Fatalf("hermes->themis = %v %q", ok, note)
	}
}

func TestParseAddress(t *testing.T) {
	for in, want := range map[string]string{
		"metis": "metis@acme", "metis@beta": "metis@beta", "@group:clean": "@group:clean@acme", "@role:reviewer@beta": "@role:reviewer@beta",
	} {
		got, err := ParseAddress(in, "acme")
		if err != nil || got.String() != want {
			t.Errorf("ParseAddress(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "@", "a@b@c", "a.b", "a@", "UPPER", "@clean", "@everyone:x", "@group:"} {
		if _, err := ParseAddress(in, "acme"); err == nil {
			t.Errorf("ParseAddress(%q) succeeded", in)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if r := l.Allow("a@x", 3, now); r != nil {
			t.Fatalf("message %d refused", i)
		}
	}
	if r := l.Allow("a@x", 3, now); r == nil || r.Code != Exhausted {
		t.Fatalf("4th message = %v; want rate refusal", r)
	}
	if r := l.Allow("b@x", 3, now); r != nil {
		t.Fatal("other sender refused")
	}
	if r := l.Allow("a@x", 3, now.Add(20*time.Second)); r != nil {
		t.Fatal("not refilled after 20s")
	}
}
