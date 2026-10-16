package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
)

type operatorTransport struct{ headerTransport }

func (o operatorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(HeaderOperatorPost, "1")
	return o.headerTransport.RoundTrip(r)
}

func TestOperatorPost(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster test")
	}
	reps := startCluster(t, 3, chatDirectory())
	ctx := context.Background()
	operator := func(id, onBehalf string) messagingv1connect.MessagingServiceClient {
		hc := &http.Client{Transport: operatorTransport{headerTransport{id, onBehalf}}}
		return messagingv1connect.NewMessagingServiceClient(hc, reps[0].http.URL)
	}
	try := func(c messagingv1connect.MessagingServiceClient, body string) error {
		_, err := c.Post(ctx, connect.NewRequest(&messagingv1.PostRequest{Id: ulid.Make().String(), Channel: "reviewers", Body: body}))
		return err
	}
	// alice exists once the control center has acted for her.
	plain := clientFor(reps[0], "control-center", "alice@chat")
	if _, err := plain.ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{})); err != nil {
		t.Fatal(err)
	}

	// Without the header a person who isn't a member is refused.
	if err := try(plain, "hello"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("plain post: %v", err)
	}
	// With it, from the control center for a person, the post goes through.
	if err := try(operator("control-center", "alice@chat"), "operators may post anywhere"); err != nil {
		t.Fatalf("operator post: %v", err)
	}
	// Rooms can't use the header.
	if err := try(operator("hermes@chat", ""), "me too"); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("room with the header: %v", err)
	}
	// Limits still apply.
	if err := try(operator("control-center", "alice@chat"), strings.Repeat("x", 1<<20)); err == nil {
		t.Fatal("an oversized operator post went through")
	}

	recs := queryAudit(t, clientFor(reps[0], "control-center", ""), &messagingv1.QueryAuditRequest{Workspace: "chat", Kinds: []string{"post"}})
	if len(recs) != 1 || recs[0].From != "alice@chat" || recs[0].To != "#reviewers" || recs[0].Reason != "operator post" {
		t.Fatalf("audit = %v", recs)
	}
}
