package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/messaging/tokenhash"
)

// TestRefusesIssuedTokens: a message or post carrying one of the
// workspace's issued tokens is refused with an error the agent can act on,
// and the token stays out of the audit (design §11.4).
func TestRefusesIssuedTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster test")
	}
	const tok = "0123456789abcdef0123456789abcdef01234567"
	dir := chatDirectory()
	ws, _ := dir.Workspace("chat")
	ws.TokenHashes = tokenhash.Parse(tokenhash.Format([]string{tok}))
	reps := startCluster(t, 3, dir)
	ctx := context.Background()
	hermes := clientFor(reps[0], "hermes@chat", "")
	refusedSecret := func(what string, err error) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "this message contains a secret token; it was not sent") {
			t.Errorf("%s: %v", what, err)
		}
	}

	_, err := hermes.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "themis", Body: "use git clone https://hermes:" + tok + "@forgejo/chat/api"}))
	refusedSecret("send", err)
	_, err = hermes.Post(ctx, connect.NewRequest(&messagingv1.PostRequest{Id: ulid.Make().String(), Channel: "quiet", Body: "token=" + tok}))
	refusedSecret("post", err)
	person := clientFor(reps[0], "control-center", "alice@chat")
	_, err = person.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "hermes", Body: tok}))
	refusedSecret("send for a person", err)

	if _, err := hermes.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: ulid.Make().String(), To: "themis", Body: "the token is in your identity Secret"})); err != nil {
		t.Fatalf("an ordinary message was refused: %v", err)
	}

	recs := queryAudit(t, clientFor(reps[0], "control-center", ""), &messagingv1.QueryAuditRequest{Workspace: "chat", Kinds: []string{"refusal"}})
	n := 0
	for _, r := range recs {
		if strings.Contains(r.Body, tok) || strings.Contains(r.Reason, tok) {
			t.Fatalf("the token is in the audit: %v", r)
		}
		if strings.HasPrefix(r.Reason, "secret:") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d secret refusals audited, want 3: %v", n, recs)
	}
}
