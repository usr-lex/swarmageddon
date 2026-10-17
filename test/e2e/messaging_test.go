//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

// Proof of delivery (design §7.2) is the recipient's side showing the
// message: the agent CLI's own session files in the room (Claude Code's
// ~/.claude/projects, ore's ~/.ore/sessions), which only the CLI writes, or
// a person's inbox (InboxService.ListInbox). A successful send proves only
// that messaging stored it. Whether the control center's Agent tab shows
// what the CLI received is a step of its own (agentTab).

// cliSaw reports whether the room's CLI recorded marker in its session files.
func (s *suite) cliSaw(room, marker string) (bool, error) {
	out, err := s.inRoom(room, "grep -rlF -e '"+marker+"' ~/.claude/projects ~/.ore/sessions 2>/dev/null | head -n 1")
	if err != nil {
		return false, fmt.Errorf("%v: %s", err, clip(out))
	}
	return strings.Contains(out, ".jsonl"), nil
}

// transcriptHas watches the room's transcript through the API for up to d
// and reports whether an item holds marker.
func (s *suite) transcriptHas(room, marker string, d time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	st, err := s.cc.rooms.WatchTranscript(ctx, connect.NewRequest(&ccv1.WatchTranscriptRequest{Room: ref(room), TailLines: 2000}))
	if err != nil {
		return false, err
	}
	defer func() { _ = st.Close() }()
	for st.Receive() {
		it := st.Msg().GetItem()
		if strings.Contains(it.GetText()+"\n"+it.GetToolInput()+"\n"+it.GetToolResult(), marker) {
			return true, nil
		}
	}
	if ctx.Err() != nil {
		return false, nil
	}
	return false, st.Err()
}

// delivered waits until room's CLI has received marker, and on failure says
// how far the message got.
func (s *suite) delivered(t *testing.T, room, marker string) {
	t.Helper()
	err := s.wait(4*time.Minute, func() error {
		ok, err := s.cliSaw(room, marker)
		if err == nil && !ok {
			err = fmt.Errorf("not in the CLI's session files")
		}
		return err
	})
	if err != nil {
		t.Fatalf("%s's CLI never received %s (%v). %s", room, marker, err, s.trace(room, marker))
	}
}

// agentTab: the room's Agent tab (RoomService.WatchTranscript, §8.4) shows
// a message the CLI received: the person's message to athena.
func (s *suite) agentTab(t *testing.T) {
	m := s.marker("admin-to-athena")
	var last error
	end := s.until(2 * time.Minute)
	for time.Now().Before(end) {
		ok, err := s.transcriptHas("athena", m, 30*time.Second)
		if ok {
			return
		}
		if last = err; err != nil {
			time.Sleep(3 * time.Second)
		}
	}
	t.Errorf("athena's CLI received %s, but her Agent tab (WatchTranscript) never showed it (last error: %v)", m, last)
}

// trace says whether marker reached room's messaging inbox and what the
// room's supervisor logged about deliveries, for a failure message.
func (s *suite) trace(room, marker string) string {
	var b strings.Builder
	out, err := s.inRoom(room, "swarm read --all --keep-unread --full --limit 50 all")
	switch {
	case err != nil:
		fmt.Fprintf(&b, "swarm read in %s: %v: %s\n", room, err, clip(out))
	case strings.Contains(out, marker):
		fmt.Fprintf(&b, "It is in %s's messaging inbox (swarm read).\n", room)
	default:
		fmt.Fprintf(&b, "It is not in %s's messaging inbox (swarm read).\n", room)
	}
	logs, _ := s.kc("-n", wsNS, "logs", podOf(room), "-c", "room", "--since=20m")
	var deliveries, warnings []string
	for _, l := range strings.Split(logs, "\n") {
		switch {
		case strings.Contains(l, "deliver"):
			deliveries = append(deliveries, l)
		case strings.Contains(l, "service exited; restarting"):
			// the display services' restarts drown everything else
		case strings.Contains(l, "level=WARN") || strings.Contains(l, "level=ERROR"):
			warnings = append(warnings, l)
		}
	}
	last := func(l []string) string {
		if len(l) > 6 {
			l = l[len(l)-6:]
		}
		return strings.Join(l, "\n")
	}
	fmt.Fprintf(&b, "%s's supervisor, last 20 min: deliveries:\n%s\nwarnings:\n%s", room, last(deliveries), last(warnings))
	return b.String()
}

// send sends from inside a room with the swarm CLI, as the agent's shell or
// MCP tool does.
func (s *suite) send(t *testing.T, from, to, body string) {
	t.Helper()
	out, err := s.inRoom(from, fmt.Sprintf("swarm send %s '%s'", to, body))
	if err != nil || !strings.Contains(out, "sent ") {
		t.Fatalf("%s: swarm send %s: %v: %s", from, to, err, clip(out))
	}
}

// direct is one agent-to-agent message, proved on the recipient's side.
func (s *suite) direct(from, to string) func(t *testing.T) {
	return func(t *testing.T) {
		m := s.marker(from + "-to-" + to)
		s.send(t, from, to, "hello from "+from+" "+m)
		s.delivered(t, to, m)
	}
}

// personToAgent sends as the signed-in admin through the inbox API.
func (s *suite) personToAgent(t *testing.T) {
	m := s.marker("admin-to-athena")
	res, err := call(s.cc.inbox.SendMessage, &ccv1.SendMessageRequest{Workspace: ws, To: "athena", Body: "hello from a person " + m})
	if err != nil {
		t.Fatalf("SendMessage to athena: %v", err)
	}
	if res.GetMessage().GetFrom() != admin+"@"+ws {
		t.Errorf("SendMessage: from %q, want %s@%s", res.GetMessage().GetFrom(), admin, ws)
	}
	s.delivered(t, "athena", m)
}

// agentToPerson sends from a room to the admin and reads the admin's inbox
// through the API.
func (s *suite) agentToPerson(t *testing.T) {
	m := s.marker("athena-to-admin")
	s.send(t, "athena", admin, "hello from athena "+m)
	s.eventually(t, 4*time.Minute, "the message in the admin's inbox", func() error {
		in, err := call(s.cc.inbox.ListInbox, &ccv1.ListInboxRequest{Workspace: ws, Limit: 100})
		if err != nil {
			return err
		}
		for _, msg := range in.GetMessages() {
			if strings.Contains(msg.GetBody(), m) {
				if msg.GetDirection() != "in" || msg.GetFrom() != "athena@"+ws {
					return fmt.Errorf("listed as %s from %q", msg.GetDirection(), msg.GetFrom())
				}
				return nil
			}
		}
		return fmt.Errorf("not among %d messages", len(in.GetMessages()))
	})
}

// mention posts to #general-clean (digest by default) with an @mention,
// which pushes to the named agent whatever its delivery mode (§7.8).
func (s *suite) mention(t *testing.T) {
	m := s.marker("mention-artemis")
	out, err := s.inRoom("hermes", fmt.Sprintf("swarm post general-clean '@artemis please look %s'", m))
	if err != nil || !strings.Contains(out, "posted ") {
		t.Fatalf("hermes: swarm post general-clean: %v: %s", err, clip(out))
	}
	s.delivered(t, "artemis", m)
}

// crossingRefused: athena may not message metis across the wall; the
// refusal names the rule, and metis never sees it (§7.4).
func (s *suite) crossingRefused(t *testing.T) {
	m := s.marker("athena-to-metis")
	out, err := s.inRoom("athena", "swarm send metis 'over the wall "+m+"'")
	if err == nil {
		t.Fatalf("athena -> metis across the wall was accepted: %s", clip(out))
	}
	if !strings.Contains(out, "only hermes may message metis") {
		t.Errorf("the refusal doesn't say which rule refused it: %s", clip(out))
	}
	s.notDelivered(t, "metis", m)
	if out, err := s.inRoom("metis", "swarm read --all --keep-unread --full --limit 50 all"); err != nil || strings.Contains(out, m) {
		t.Errorf("metis's inbox: %v; holds the refused message: %v", err, strings.Contains(out, m))
	}
	out, err = s.inRoom("athena", "swarm agents")
	if err != nil || !strings.Contains(out, "hermes@"+ws) || strings.Contains(out, "metis@"+ws) {
		t.Errorf("athena's peers should list hermes and not metis: %v:\n%s", err, clip(out))
	}
	s.audited(t, "refusal", "athena", "metis")
}

// crossingMediated: hermes is the mediated link's sender to metis.
func (s *suite) crossingMediated(t *testing.T) {
	m := s.marker("hermes-to-metis")
	s.send(t, "hermes", "metis", "allowed crossing "+m)
	s.delivered(t, "metis", m)
	s.audited(t, "crossing", "hermes", "metis")
}

// tokenRefused: a message carrying one of the workspace's issued tokens (here
// athena's own forge token) is refused and never delivered (§11.4).
func (s *suite) tokenRefused(t *testing.T) {
	m := s.marker("token-leak")
	out, err := s.inRoomAsRoot("athena", `swarm send hermes "`+m+` $(cat /run/swarm/identity/forge_token)"`)
	if err == nil {
		t.Fatalf("a message with athena's forge token was accepted: %s", clip(out))
	}
	if !strings.Contains(out, "contains a secret token") {
		t.Errorf("the refusal doesn't say why: %s", clip(out))
	}
	s.notDelivered(t, "hermes", m)
}

// notDelivered checks, after giving a refused message time to arrive anyway,
// that room's CLI never received marker.
func (s *suite) notDelivered(t *testing.T, room, marker string) {
	t.Helper()
	time.Sleep(15 * time.Second)
	if ok, err := s.cliSaw(room, marker); err != nil || ok {
		t.Errorf("%s's CLI received the refused message %s (%v)", room, marker, err)
	}
}

// audited waits for a messaging audit record of kind from one room to another,
// through the control center's audit view.
func (s *suite) audited(t *testing.T, kind, from, to string) {
	t.Helper()
	s.eventually(t, 2*time.Minute, kind+" in the audit log", func() error {
		res, err := call(s.cc.audit.QueryAudit, &ccv1.QueryAuditRequest{Source: ccv1.AuditSource_AUDIT_SOURCE_MESSAGING,
			Workspace: ws, Since: timestamppb.New(s.start), Actions: []string{kind}, Limit: 500})
		if err != nil {
			return err
		}
		if slices.ContainsFunc(res.GetEntries(), func(e *ccv1.AuditEntry) bool {
			return strings.HasPrefix(e.GetActor(), from) && strings.Contains(e.GetObject(), to)
		}) {
			return nil
		}
		return fmt.Errorf("no %s from %s to %s among %d entries", kind, from, to, len(res.GetEntries()))
	})
}
