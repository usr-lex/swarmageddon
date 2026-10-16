package supervisor

import (
	"fmt"
	"strings"
	"testing"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

func msg(id, from, body string) *messagingv1.Delivery {
	return &messagingv1.Delivery{Item: &messagingv1.Delivery_Message{Message: &messagingv1.Message{Id: id, From: from, Body: body}}}
}

func TestCoalesceSingleMessage(t *testing.T) {
	got := Coalesce([]*messagingv1.Delivery{msg("01A", "hermes@acme", "review #3 please")}, 4096)
	if !strings.HasPrefix(got, `<cross-session-message from="hermes@acme"`) || !strings.Contains(got, "review #3 please") {
		t.Errorf("single: %s", got)
	}
}

func TestCoalesceManyItems(t *testing.T) {
	long := strings.Repeat("a line of a long report\n", 400)
	items := []*messagingv1.Delivery{
		msg("01A", "alice@acme", "first"),
		{Item: &messagingv1.Delivery_Post{Post: &messagingv1.ChannelPost{Id: "01P", Channel: "reviewers", From: "hermes@acme", Body: long}}},
		{Item: &messagingv1.Delivery_Digest{Digest: &messagingv1.Digest{Channel: "general", NewPosts: 7, Posters: []string{"a", "b"}, Previews: []string{"hello\nmore"}}}},
		{Item: &messagingv1.Delivery_Notice{Notice: &messagingv1.Notice{Id: "n1", Text: "3 more messages are waiting"}}},
	}
	got := Coalesce(items, 4096)
	if len(got) > 4096 {
		t.Errorf("over the cap: %d bytes", len(got))
	}
	for _, want := range []string{`<swarm-messages count="4">`, "[direct] from alice@acme, id 01A:\nfirst", "[#reviewers] from hermes@acme, id 01P:",
		"read the rest with get_message 01P", "[#general digest] 7 new posts from a, b:\n- hello\n", "read_messages scope=general", "[notice] 3 more messages"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestCoalesceOverflowCounts(t *testing.T) {
	var items []*messagingv1.Delivery
	for i := range 100 {
		items = append(items, msg(fmt.Sprint(i), "x@acme", strings.Repeat("y", 100)))
	}
	got := Coalesce(items, 4096)
	if len(got) > 4096 || !strings.Contains(got, "more items are waiting; read them with read_messages") {
		t.Errorf("overflow (%d bytes):\n%s", len(got), got)
	}
}
