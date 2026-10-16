package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
)

// chatDirectory: clean and ops are joined by open links both ways; dirty is
// behind mediated links (hermes may reach metis).
func chatDirectory() *directory.Static {
	member := func(sel string, a v1alpha1.ChannelAccess) v1alpha1.ChannelMember {
		return v1alpha1.ChannelMember{Select: sel, Access: a}
	}
	ws := &directory.Workspace{
		Name:            "chat",
		Groups:          []string{"clean", "dirty", "ops"},
		ApprovalTimeout: time.Hour,
		Roles:           map[string]bool{"coordinator": true, "reviewer": true},
		Teams:           map[string]bool{"mergers": true},
		Messaging: v1alpha1.MessagingPolicy{
			Links: []v1alpha1.GroupLink{
				{From: "clean", To: "ops", Mode: v1alpha1.LinkOpen},
				{From: "ops", To: "clean", Mode: v1alpha1.LinkOpen},
				{From: "clean", To: "dirty", Mode: v1alpha1.LinkMediated, Senders: []string{"hermes"}, Recipients: []string{"metis"}},
			},
			Limits: v1alpha1.MessageLimits{RatePerMinute: 1000},
			Storm: v1alpha1.StormControl{PushPerMinute: 5, FanoutPerMinute: 3,
				DigestInterval: &metav1.Duration{Duration: 2 * time.Second}},
			Channels: []v1alpha1.ChannelSpec{
				{Name: "announcements", DefaultDelivery: v1alpha1.DeliveryPush, Members: []v1alpha1.ChannelMember{
					member("everyone", v1alpha1.AccessRead), member("people", v1alpha1.AccessWrite),
					member("role:coordinator", v1alpha1.AccessWrite)}},
				{Name: "reviewers", Members: []v1alpha1.ChannelMember{member("role:reviewer", v1alpha1.AccessReadWrite)}},
				{Name: "quiet", DefaultDelivery: v1alpha1.DeliveryPull,
					AllowedDelivery: []v1alpha1.DeliveryMode{v1alpha1.DeliveryPull, v1alpha1.DeliveryDigest},
					Members:         []v1alpha1.ChannelMember{member("everyone", v1alpha1.AccessReadWrite)}},
			},
		},
		Rooms: map[string]directory.Room{
			"hermes": {Name: "hermes", Group: "clean", Role: "coordinator"},
			"themis": {Name: "themis", Group: "clean"},
			"athena": {Name: "athena", Group: "clean", Role: "reviewer", Teams: []string{"mergers"}},
			"apollo": {Name: "apollo", Group: "clean", Role: "reviewer"},
			"atlas":  {Name: "atlas", Group: "ops", Role: "reviewer", Teams: []string{"mergers"}},
			"metis":  {Name: "metis", Group: "dirty", Role: "reviewer", Teams: []string{"mergers"}},
		},
	}
	return directory.NewStatic(ws)
}

func post(t *testing.T, c messagingv1connect.MessagingServiceClient, channel, body string) *messagingv1.PostResponse {
	t.Helper()
	var res *connect.Response[messagingv1.PostResponse]
	var err error
	id := ulid.Make().String()
	for range 40 { // retried with the same id, like a supervisor
		res, err = c.Post(context.Background(), connect.NewRequest(&messagingv1.PostRequest{Id: id, Channel: channel, Body: body}))
		if err == nil || connect.CodeOf(err) != connect.CodeUnavailable {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("post to #%s: %v", channel, err)
	}
	return res.Msg
}

// waitFor acks deliveries until one matches.
func (in *inbox) waitFor(t *testing.T, what string, match func(*messagingv1.Delivery) bool) *messagingv1.Delivery {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case d, ok := <-in.ch:
			if !ok {
				t.Fatalf("inbox closed waiting for %s", what)
			}
			ackRetry(t, in, d)
			if match(d) {
				return d
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func read(t *testing.T, c messagingv1connect.MessagingServiceClient, r *messagingv1.ReadMessagesRequest) *messagingv1.ReadMessagesResponse {
	t.Helper()
	res, err := c.ReadMessages(context.Background(), connect.NewRequest(r))
	if err != nil {
		t.Fatalf("read_messages %v: %v", r, err)
	}
	return res.Msg
}

func bodies(items []*messagingv1.InboxItem) string {
	var out []string
	for _, it := range items {
		out = append(out, it.Body)
	}
	return strings.Join(out, ",")
}

func TestChannels(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster test")
	}
	reps := startCluster(t, 3, chatDirectory())
	ctx := context.Background()
	client := func(i int, name string) messagingv1connect.MessagingServiceClient {
		return clientFor(reps[i], name+"@chat", "")
	}
	hermes, atlas := client(0, "hermes"), client(1, "atlas")
	alice := clientFor(reps[2], "control-center", "alice@chat")
	// Reviewers that aren't under test read #reviewers by pulling.
	for _, c := range []messagingv1connect.MessagingServiceClient{client(0, "athena"), atlas} {
		if _, err := c.SetDelivery(ctx, connect.NewRequest(&messagingv1.SetDeliveryRequest{Channel: "reviewers", Mode: "pull"})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client(1, "themis").SetDelivery(ctx, connect.NewRequest(&messagingv1.SetDeliveryRequest{Channel: "announcements", Mode: "pull"})); err != nil {
		t.Fatal(err)
	}
	// alice exists once the control center has acted for her.
	if _, err := alice.ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{})); err != nil {
		t.Fatal(err)
	}

	athena := subscribe(t, client(0, "athena"))
	var apollo *inbox
	{
		// apollo listens on a replica that doesn't lead the inbox stream,
		// so the failover test can stop the leader under it.
		leader := reps[0].st.InboxLeader(ctx, "chat")
		for i, r := range reps {
			if r.node.Name() != leader {
				apollo = subscribe(t, client(i, "apollo"))
				break
			}
		}
	}

	t.Run("access and walls", func(t *testing.T) {
		_, err := hermes.Post(ctx, connect.NewRequest(&messagingv1.PostRequest{Id: ulid.Make().String(), Channel: "reviewers", Body: "hi"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "not a member of #reviewers") {
			t.Fatalf("hermes posting to #reviewers: %v", err)
		}
		_, err = client(0, "athena").Post(ctx, connect.NewRequest(&messagingv1.PostRequest{Id: ulid.Make().String(), Channel: "announcements", Body: "hi"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "may read #announcements but not post") {
			t.Fatalf("athena posting to #announcements: %v", err)
		}
		// metis is behind a mediated link: not a member of anything.
		_, err = client(1, "metis").ReadMessages(ctx, connect.NewRequest(&messagingv1.ReadMessagesRequest{Scope: "reviewers"}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("metis reading #reviewers: %v", err)
		}
		res, err := client(1, "metis").ListChannels(ctx, connect.NewRequest(&messagingv1.ListChannelsRequest{}))
		if err != nil || len(res.Msg.Channels) != 0 {
			t.Fatalf("metis channels %v %v", res, err)
		}
		lc, err := alice.ListChannels(ctx, connect.NewRequest(&messagingv1.ListChannelsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range lc.Msg.Channels {
			got = append(got, c.Name+":"+c.Access+":"+c.Delivery)
		}
		if want := "announcements:readWrite:push,quiet:readWrite:pull"; strings.Join(got, ",") != want {
			t.Fatalf("alice's channels %v; want %s", got, want)
		}
		_, err = alice.SetDelivery(ctx, connect.NewRequest(&messagingv1.SetDeliveryRequest{Channel: "quiet", Mode: "push"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("push on #quiet: %v", err)
		}
	})

	var firstDigest time.Time
	t.Run("post with a mention pushes", func(t *testing.T) {
		res := post(t, atlas, "reviewers", "@athena please review #4; @metis can't see this")
		if strings.Join(res.Mentioned, ",") != "athena@chat" {
			t.Fatalf("mentioned %v", res.Mentioned)
		}
		d := athena.waitFor(t, "the mention", func(d *messagingv1.Delivery) bool { return d.GetPost() != nil })
		if p := d.GetPost(); p.Channel != "reviewers" || p.From != "atlas@chat" || !strings.HasPrefix(p.Body, "@athena") {
			t.Fatalf("post %v", p)
		}
		// A pushed and acked post counts as read.
		time.Sleep(200 * time.Millisecond)
		if r := read(t, client(0, "athena"), &messagingv1.ReadMessagesRequest{Scope: "reviewers"}); len(r.Items) != 0 {
			t.Fatalf("athena unread in #reviewers: %v", r.Items)
		}
		// apollo reads #reviewers as a digest.
		d = apollo.waitFor(t, "the first digest", func(d *messagingv1.Delivery) bool { return d.GetDigest() != nil })
		firstDigest = time.Now()
		if g := d.GetDigest(); g.Channel != "reviewers" || g.NewPosts != 1 || g.Posters[0] != "atlas@chat" {
			t.Fatalf("digest %v", g)
		}
		// The same text again is refused; the same id is a duplicate.
		_, err := atlas.Post(ctx, connect.NewRequest(&messagingv1.PostRequest{Id: ulid.Make().String(), Channel: "reviewers",
			Body: "@athena please review #4; @metis can't see this"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "already sent this exact text") {
			t.Fatalf("repeat post: %v", err)
		}
		// Posts to a push channel reach every reader.
		post(t, hermes, "announcements", "deploy freeze at 5")
		for _, in := range []*inbox{athena, apollo} {
			in.waitFor(t, "the announcement", func(d *messagingv1.Delivery) bool { return d.GetPost().GetBody() == "deploy freeze at 5" })
		}
		recs := queryAudit(t, clientFor(reps[0], "control-center", ""), &messagingv1.QueryAuditRequest{Workspace: "chat", Kinds: []string{"post"}})
		if len(recs) != 2 || recs[1].To != "#announcements" {
			t.Fatalf("post audit %v", recs)
		}
	})

	t.Run("digest at most once per interval", func(t *testing.T) {
		// The digest tick is 300ms and the interval 2s: without the
		// interval, "second" would get a digest of its own.
		post(t, atlas, "reviewers", "second")
		time.Sleep(time.Second)
		post(t, atlas, "reviewers", "third")
		d := apollo.waitFor(t, "the second digest", func(d *messagingv1.Delivery) bool { return d.GetDigest() != nil })
		if g := d.GetDigest(); g.NewPosts != 2 || strings.Join(g.Previews, ",") != "second,third" {
			t.Fatalf("digest %v", g)
		}
		if since := time.Since(firstDigest); since < 1500*time.Millisecond {
			t.Fatalf("second digest %s after the first; the interval is 2s", since)
		}
	})

	t.Run("push budget and urgent bypass", func(t *testing.T) {
		for i := range 8 {
			send(t, hermes, ulid.Make().String(), "themis", fmt.Sprint("dm ", i))
		}
		themis := subscribe(t, client(1, "themis"))
		for i := range 5 {
			if d := themis.next(t, true); d.GetMessage().GetBody() != fmt.Sprint("dm ", i) {
				t.Fatalf("pushed %v", d)
			}
		}
		n := themis.next(t, true).GetNotice()
		if n.GetText() != "3 more messages are waiting; read them with read_messages" {
			t.Fatalf("notice %v", n)
		}
		send(t, alice, ulid.Make().String(), "themis", "from a person")
		if d := themis.next(t, true); d.GetMessage().GetBody() != "from a person" || !d.Urgent {
			t.Fatalf("urgent %v", d)
		}
		send(t, hermes, ulid.Make().String(), "themis", "held too")
		themis.none(t, 500*time.Millisecond)
		time.Sleep(200 * time.Millisecond)
		r := read(t, client(1, "themis"), &messagingv1.ReadMessagesRequest{Scope: "direct"})
		if got := bodies(r.Items); got != "dm 5,dm 6,dm 7,held too" {
			t.Fatalf("unread direct %q", got)
		}
	})

	t.Run("group send across a mediated link", func(t *testing.T) {
		metis := subscribe(t, client(2, "metis"))
		atlasIn := subscribe(t, atlas)
		res := send(t, hermes, ulid.Make().String(), "@team:mergers", "who can merge #4?")
		if res.Duplicate {
			t.Fatal("duplicate")
		}
		for _, in := range []*inbox{athena, atlasIn} {
			d := in.waitFor(t, "the group send", func(d *messagingv1.Delivery) bool { return d.GetMessage() != nil })
			if m := d.GetMessage(); m.Body != "who can merge #4?" || m.From != "hermes@chat" {
				t.Fatalf("message %v", m)
			}
		}
		metis.none(t, time.Second)
		recs := queryAudit(t, clientFor(reps[0], "control-center", ""), &messagingv1.QueryAuditRequest{Workspace: "chat",
			Kinds: []string{"refusal"}, Address: "metis@chat"})
		if len(recs) != 1 || !strings.HasPrefix(recs[0].Reason, "mediated-group") {
			t.Fatalf("refusals %v", recs)
		}
		// The old @<group> form is gone; the fan-out budget is 3 a minute.
		_, err := hermes.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "@clean", Body: "x"}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("@clean: %v", err)
		}
		_, err = hermes.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "@role:reviewer", Body: "y"}))
		if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "fan-out budget") {
			t.Fatalf("over fan-out: %v", err)
		}
	})

	t.Run("pagination and cursors", func(t *testing.T) {
		long := strings.Repeat("x", 600)
		for i := range 6 {
			post(t, hermes, "quiet", fmt.Sprint("q", i))
		}
		post(t, hermes, "quiet", long)
		f := false
		r1 := read(t, alice, &messagingv1.ReadMessagesRequest{Scope: "#quiet", Limit: 3, MarkRead: &f})
		if bodies(r1.Items) != "q0,q1,q2" || r1.UnreadRemaining != 4 || r1.NextCursor == "" {
			t.Fatalf("page 1 %q %d %q", bodies(r1.Items), r1.UnreadRemaining, r1.NextCursor)
		}
		r2 := read(t, alice, &messagingv1.ReadMessagesRequest{Scope: "quiet", Limit: 3, Cursor: r1.NextCursor})
		if bodies(r2.Items) != "q3,q4,q5" || r2.UnreadRemaining != 4 {
			t.Fatalf("page 2 %q %d", bodies(r2.Items), r2.UnreadRemaining)
		}
		r3 := read(t, alice, &messagingv1.ReadMessagesRequest{Scope: "quiet", Limit: 3})
		if bodies(r3.Items) != "q0,q1,q2" || r3.UnreadRemaining != 1 {
			t.Fatalf("page 3 %q %d", bodies(r3.Items), r3.UnreadRemaining)
		}
		r4 := read(t, alice, &messagingv1.ReadMessagesRequest{Scope: "quiet"})
		if len(r4.Items) != 1 || !r4.Items[0].Truncated || len(r4.Items[0].Body) != 500 || r4.NextCursor != "" || r4.UnreadRemaining != 0 {
			t.Fatalf("page 4 %v", r4)
		}
		got, err := alice.GetMessage(ctx, connect.NewRequest(&messagingv1.GetMessageRequest{Id: r4.Items[0].Id}))
		if err != nil || got.Msg.Item.Body != long || !got.Msg.Item.Read {
			t.Fatalf("get_message %v %v", got, err)
		}
		// History, newest last, paging backwards.
		h1 := read(t, alice, &messagingv1.ReadMessagesRequest{Scope: "quiet", UnreadOnly: &f, Limit: 4})
		if bodies(h1.Items) != "q3,q4,q5,"+long[:500] || h1.NextCursor == "" {
			t.Fatalf("history 1 %q", bodies(h1.Items))
		}
		h2 := read(t, alice, &messagingv1.ReadMessagesRequest{Scope: "quiet", UnreadOnly: &f, Limit: 4, Cursor: h1.NextCursor})
		if bodies(h2.Items) != "q0,q1,q2" || h2.NextCursor != "" {
			t.Fatalf("history 2 %q %q", bodies(h2.Items), h2.NextCursor)
		}
		// Unread summary and mark_read for a person, through the control center.
		post(t, hermes, "quiet", "@alice lunch?")
		post(t, hermes, "announcements", "standup moved")
		sum, err := alice.UnreadSummary(ctx, connect.NewRequest(&messagingv1.UnreadSummaryRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int32{}
		for _, c := range sum.Msg.Counts {
			counts[c.Scope] = c.Unread
		}
		if counts["quiet"] != 1 || counts["mentions"] != 1 || counts["announcements"] != 2 || counts["direct"] != 0 {
			t.Fatalf("summary %v", counts)
		}
		m := read(t, alice, &messagingv1.ReadMessagesRequest{Scope: "mentions"})
		if bodies(m.Items) != "@alice lunch?" || !m.Items[0].MentionsMe {
			t.Fatalf("mentions %v", m.Items)
		}
		if _, err := alice.MarkRead(ctx, connect.NewRequest(&messagingv1.MarkReadRequest{Scope: "all"})); err != nil {
			t.Fatal(err)
		}
		if r := read(t, alice, &messagingv1.ReadMessagesRequest{}); len(r.Items) != 0 || r.UnreadRemaining != 0 {
			t.Fatalf("after mark_read %v", r)
		}
		if _, err := alice.ReadMessages(ctx, connect.NewRequest(&messagingv1.ReadMessagesRequest{Cursor: "junk"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("bad cursor: %v", err)
		}
	})

	t.Run("digest leader failover", func(t *testing.T) {
		leader := reps[0].st.InboxLeader(ctx, "chat")
		var li int
		fmt.Sscanf(leader, "messaging-%d", &li)
		poster := client((li+1)%3, "atlas")
		reps[li].close()
		reps[li] = nil
		time.Sleep(2 * time.Second) // past the digest interval
		post(t, poster, "reviewers", "after failover")
		d := apollo.waitFor(t, "a digest from the new leader", func(d *messagingv1.Delivery) bool { return d.GetDigest() != nil })
		if g := d.GetDigest(); g.NewPosts != 1 || g.Previews[0] != "after failover" {
			t.Fatalf("digest %v", g)
		}
	})
}

func TestMentions(t *testing.T) {
	got := mentions("@athena and @apollo, mail me@example.com, @role:reviewer @Bob @athena (@atlas)")
	if strings.Join(got, ",") != "athena,apollo,atlas" {
		t.Fatalf("mentions %v", got)
	}
	if firstLine("\n  hello world\nmore", 5) != "hello…" {
		t.Fatal(firstLine("\n  hello world\nmore", 5))
	}
	c := &cursor{Unread: true, Pos: map[string]uint64{"direct": 4}}
	back, err := decodeCursor(c.encode())
	if err != nil || back.Pos["direct"] != 4 || !back.Unread {
		t.Fatalf("cursor %v %v", back, err)
	}
}
