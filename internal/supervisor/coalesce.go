package supervisor

import (
	"fmt"
	"strings"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

// DefaultCoalesceBytes caps one injected message (design §7.8).
const DefaultCoalesceBytes = 4096

// Coalesce turns pushed items (messages, posts, digests, notices) into one
// text for the agent, at most about limit bytes. Long bodies are cut to
// their first lines with a pointer to get_message; items beyond the cap are
// counted with a pointer to read_messages. Approval answers are not items.
func Coalesce(items []*messagingv1.Delivery, limit int) string {
	if limit <= 0 {
		limit = DefaultCoalesceBytes
	}
	if len(items) == 1 && items[0].GetMessage() != nil {
		m := items[0].GetMessage()
		body := cut(m.GetBody(), limit-300, m.GetId())
		return agent.Envelope(m.GetFrom(), m.GetFromKind(), m.GetReplyTo(), m.GetId(), body)
	}
	header := fmt.Sprintf("<swarm-messages count=\"%d\">\nNew messages for you. Reply with send_message (direct) or post (channel); read_messages shows more.\n", len(items))
	footer := "</swarm-messages>"
	budget := limit - len(header) - len(footer)
	per := max(budget/len(items), 240)
	var b strings.Builder
	b.WriteString(header)
	for i, d := range items {
		block := formatItem(d, per)
		if b.Len()+len(block) > limit-len(footer)-80 && i > 0 {
			fmt.Fprintf(&b, "\n%d more items are waiting; read them with read_messages.\n", len(items)-i)
			break
		}
		b.WriteString("\n" + block)
	}
	b.WriteString(footer)
	return b.String()
}

func formatItem(d *messagingv1.Delivery, budget int) string {
	switch {
	case d.GetMessage() != nil:
		m := d.GetMessage()
		who := m.GetFrom()
		if m.GetFromKind() == "human" {
			who += " (a person)"
		}
		reply := ""
		if m.GetReplyTo() != "" && m.GetReplyTo() != m.GetFrom() {
			reply = ", reply to " + m.GetReplyTo()
		}
		return fmt.Sprintf("[direct] from %s%s, id %s:\n%s\n", who, reply, m.GetId(), cut(m.GetBody(), budget, m.GetId()))
	case d.GetPost() != nil:
		p := d.GetPost()
		body := cut(p.GetBody(), budget, p.GetId())
		if p.GetTruncated() && !strings.Contains(body, "get_message") {
			body += "\n[read the rest with get_message " + p.GetId() + "]"
		}
		return fmt.Sprintf("[#%s] from %s, id %s:\n%s\n", p.GetChannel(), p.GetFrom(), p.GetId(), body)
	case d.GetDigest() != nil:
		g := d.GetDigest()
		var b strings.Builder
		fmt.Fprintf(&b, "[#%s digest] %d new posts from %s:\n", g.GetChannel(), g.GetNewPosts(), strings.Join(g.GetPosters(), ", "))
		for _, p := range g.GetPreviews() {
			b.WriteString("- " + firstLine(p) + "\n")
		}
		fmt.Fprintf(&b, "Read them with read_messages scope=%s.\n", g.GetChannel())
		return b.String()
	case d.GetNotice() != nil:
		return "[notice] " + d.GetNotice().GetText() + "\n"
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// cut keeps whole first lines of body within budget bytes.
func cut(body string, budget int, id string) string {
	if budget < 200 {
		budget = 200
	}
	if len(body) <= budget {
		return body
	}
	var b strings.Builder
	for _, line := range strings.SplitAfter(body, "\n") {
		if b.Len()+len(line) > budget-60 {
			break
		}
		b.WriteString(line)
	}
	if b.Len() == 0 {
		b.WriteString(strings.ToValidUTF8(body[:budget-60], ""))
	}
	return strings.TrimRight(b.String(), "\n") + "\n[... read the rest with get_message " + id + "]"
}
