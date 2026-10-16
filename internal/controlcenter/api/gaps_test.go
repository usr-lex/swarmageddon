package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1/controlcenterv1connect"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/msgclient"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store/storetest"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/terminal"
)

// fakeExec records commands and answers Run with canned output.
type fakeExec struct {
	mu     sync.Mutex
	cmds   [][]string
	out    string
	follow bool // block after writing, like tail -F
	err    error
}

func (f *fakeExec) Exec(context.Context, string, string, string, []string, io.Reader, io.Writer, <-chan terminal.Size) error {
	return nil
}

func (f *fakeExec) Run(ctx context.Context, ns, pod, container string, cmd []string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.cmds = append(f.cmds, append([]string{ns, pod, container}, cmd...))
	f.mu.Unlock()
	io.WriteString(stdout, f.out)
	if f.follow {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.err
}

func (f *fakeExec) last() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cmds[len(f.cmds)-1]
}

// signIn creates a database user with a role and workspace assignments.
func signIn(t *testing.T, e *env, db *store.Store, handle, role string, assigned ...string) *http.Client {
	t.Helper()
	ctx := context.Background()
	u, err := db.SignIn(ctx, store.Identity{Provider: "oidc:kc", Subject: handle, Handle: handle, Email: handle + "@example.com", MappedRole: role, DisplayName: strings.ToUpper(handle)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAssignments(ctx, u.ID, assigned); err != nil {
		t.Fatal(err)
	}
	return e.as(t, session.Data{UserID: u.ID})
}

func openDB(t *testing.T, e *env) *store.Store {
	t.Helper()
	db, err := store.Open(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	e.srv.SetDB(db)
	return db
}

func audited(t *testing.T, db *store.Store, action string) store.AuditEvent {
	t.Helper()
	evs, err := db.QueryAudit(context.Background(), store.AuditFilter{Actions: []string{action}})
	if err != nil || len(evs) == 0 {
		t.Fatalf("no audit for %s: %v", action, err)
	}
	return evs[0]
}

func TestIsolationAndRerunPreflight(t *testing.T) {
	e := newEnv(t)
	db := openDB(t, e)
	ctx := context.Background()
	admin := controlcenterv1connect.NewPlatformServiceClient(signIn(t, e, db, "root", "admin"), e.ts.URL+"/api")
	op := controlcenterv1connect.NewPlatformServiceClient(signIn(t, e, db, "olga", "operator"), e.ts.URL+"/api")

	if _, err := op.SetIsolation(ctx, connect.NewRequest(&ccv1.SetIsolationRequest{Tier: "userns"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("operator set the tier: %v", err)
	}
	if _, err := op.RerunPreflight(ctx, connect.NewRequest(&ccv1.RerunPreflightRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("operator re-ran preflight: %v", err)
	}
	if _, err := admin.SetIsolation(ctx, connect.NewRequest(&ccv1.SetIsolationRequest{Tier: "privileged"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("privileged without acceptance: %v", err)
	}
	if _, err := admin.SetIsolation(ctx, connect.NewRequest(&ccv1.SetIsolationRequest{Tier: "chroot"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown tier: %v", err)
	}
	res, err := admin.SetIsolation(ctx, connect.NewRequest(&ccv1.SetIsolationRequest{Tier: "privileged", PrivilegedAccepted: true}))
	if err != nil || res.Msg.Platform.IsolationTier != "privileged" || !res.Msg.Platform.PrivilegedAccepted {
		t.Fatalf("set: %+v %v", res, err)
	}
	var p v1alpha1.Platform
	e.c.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p)
	if p.Spec.IsolationTier != v1alpha1.TierPrivileged || !p.Spec.PrivilegedAccepted || p.Annotations[AnnotationRerunPreflight] == "" {
		t.Fatalf("platform %+v %v", p.Spec, p.Annotations)
	}
	if ev := audited(t, db, "platform.isolation"); ev.Actor != "root@example.com" || !strings.Contains(ev.After, `"privilegedAccepted":true`) {
		t.Fatalf("audit %+v", ev)
	}

	first := p.Annotations[AnnotationRerunPreflight]
	time.Sleep(time.Millisecond)
	rr, err := admin.RerunPreflight(ctx, connect.NewRequest(&ccv1.RerunPreflightRequest{}))
	if err != nil || rr.Msg.Requested == first {
		t.Fatalf("rerun %+v %v", rr, err)
	}
	// The operator records what it handled; the API shows both.
	e.c.Get(ctx, types.NamespacedName{Name: v1alpha1.PlatformName}, &p)
	p.Status.PreflightRerun = first
	if err := e.c.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	got, err := admin.GetPlatform(ctx, connect.NewRequest(&ccv1.GetPlatformRequest{}))
	if err != nil || got.Msg.Platform.Status.PreflightRerunRequested != rr.Msg.Requested || got.Msg.Platform.Status.PreflightRerun != first {
		t.Fatalf("status %+v %v", got.Msg.Platform.GetStatus(), err)
	}
	if ev := audited(t, db, "platform.rerunPreflight"); ev.After != rr.Msg.Requested {
		t.Fatalf("audit %+v", ev)
	}
}

func TestTranscriptAndPrune(t *testing.T) {
	r := room("acme", "athena")
	r.Spec.Repos = []string{"acme/api"}
	e := newEnv(t, workspace("acme", "owner@example.com"), r)
	db := openDB(t, e)
	ctx := context.Background()
	fx := &fakeExec{follow: true, out: `{"type":"user","uuid":"u1","message":{"role":"user","content":"hello"}}
{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}
`}
	e.srv.Exec = fx
	viewer := controlcenterv1connect.NewRoomServiceClient(signIn(t, e, db, "vic", "viewer", "acme"), e.ts.URL+"/api")
	owner := controlcenterv1connect.NewRoomServiceClient(signIn(t, e, db, "owner", "operator"), e.ts.URL+"/api")
	outsider := controlcenterv1connect.NewRoomServiceClient(signIn(t, e, db, "otto", "viewer"), e.ts.URL+"/api")
	ref := &ccv1.RoomRef{Workspace: "acme", Name: "athena"}

	// Viewers may read the transcript.
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := viewer.WatchTranscript(sctx, connect.NewRequest(&ccv1.WatchTranscriptRequest{Room: ref}))
	if err != nil {
		t.Fatal(err)
	}
	var items []*ccv1.TranscriptItem
	for len(items) < 2 && st.Receive() {
		items = append(items, st.Msg().Item)
	}
	st.Close()
	if len(items) != 2 || items[0].Text != "hello" || items[1].ToolName != "Bash" || items[1].ToolInput != "ls" {
		t.Fatalf("items %+v %v", items, st.Err())
	}
	cmd := strings.Join(fx.last(), " ")
	if !strings.HasPrefix(cmd, "ws-acme room-athena-0 room runuser -u athena -- ") || !strings.HasSuffix(cmd, "-home-athena-work-api 200 claude") {
		t.Fatalf("command %s", cmd)
	}
	st, _ = outsider.WatchTranscript(ctx, connect.NewRequest(&ccv1.WatchTranscriptRequest{Room: ref}))
	if st.Receive() || code(st.Err()) != connect.CodePermissionDenied {
		t.Fatalf("outsider read the transcript: %v", st.Err())
	}

	// Pruning: operators only, audited, reclaimed space parsed.
	fx.follow = false
	fx.out = "Deleted Images:\nuntagged: x\n\nTotal reclaimed space: 1.5GB\n"
	if _, err := viewer.PruneEngine(ctx, connect.NewRequest(&ccv1.PruneEngineRequest{Room: ref, Kind: "all"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer pruned: %v", err)
	}
	if _, err := owner.PruneEngine(ctx, connect.NewRequest(&ccv1.PruneEngineRequest{Room: ref, Kind: "everything"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad kind: %v", err)
	}
	res, err := owner.PruneEngine(ctx, connect.NewRequest(&ccv1.PruneEngineRequest{Room: ref, Kind: "all", Volumes: true}))
	if err != nil || res.Msg.ReclaimedBytes != 1_500_000_000 || !strings.Contains(res.Msg.Output, "Total reclaimed") {
		t.Fatalf("prune %+v %v", res, err)
	}
	if got := strings.Join(fx.last()[3:], " "); got != "runuser -u athena -- docker system prune -f --volumes" {
		t.Fatalf("prune command %s", got)
	}
	if ev := audited(t, db, "room.prune"); ev.Workspace != "acme" || !strings.Contains(ev.After, "system prune -f --volumes") {
		t.Fatalf("audit %+v", ev)
	}
	fx.err, fx.out = fmt.Errorf("exit 1"), "Cannot connect to the Docker daemon"
	if _, err := owner.PruneEngine(ctx, connect.NewRequest(&ccv1.PruneEngineRequest{Room: ref, Kind: "images"})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "Docker daemon") {
		t.Fatalf("failed prune: %v", err)
	}
}

func TestReclaimed(t *testing.T) {
	out := "Total reclaimed space: 512kB\nTotal: 2MB\nTotal reclaimed space: 0B\n"
	if n := reclaimed(out); n != 2_512_000 {
		t.Fatalf("reclaimed %d", n)
	}
}

// chanMessaging is a messaging fake for reading and peers.
type chanMessaging struct {
	messagingv1connect.UnimplementedMessagingServiceHandler
	mu    sync.Mutex
	items []*messagingv1.InboxItem
	asWho map[string]bool
	reads []*messagingv1.ReadMessagesRequest
}

func (f *chanMessaging) ReadMessages(_ context.Context, r *connect.Request[messagingv1.ReadMessagesRequest]) (*connect.Response[messagingv1.ReadMessagesResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asWho[r.Header().Get(msgclient.OnBehalfHeader)] = true
	f.reads = append(f.reads, r.Msg)
	return connect.NewResponse(&messagingv1.ReadMessagesResponse{Items: append([]*messagingv1.InboxItem(nil), f.items...)}), nil
}

func (f *chanMessaging) ListPeers(_ context.Context, r *connect.Request[messagingv1.ListPeersRequest]) (*connect.Response[messagingv1.ListPeersResponse], error) {
	f.mu.Lock()
	f.asWho[r.Header().Get(msgclient.OnBehalfHeader)] = true
	f.mu.Unlock()
	return connect.NewResponse(&messagingv1.ListPeersResponse{Self: "vic@acme", Peers: []*messagingv1.Peer{
		{Address: "athena@acme", Kind: "agent", DisplayName: "Athena", Title: "Coordinator", Group: "clean", Presence: messagingv1.Presence_PRESENCE_WORKING},
		{Address: "owner@acme", Kind: "human", DisplayName: "The owner"},
		{Address: "zeus@other", Kind: "agent"},
	}}), nil
}

func (f *chanMessaging) add(it *messagingv1.InboxItem) {
	f.mu.Lock()
	f.items = append(f.items, it)
	f.mu.Unlock()
}

func TestChannelStreamAndMentionables(t *testing.T) {
	e := newEnv(t, workspace("acme", "owner@example.com"), workspace("beta"))
	db := openDB(t, e)
	ctx := context.Background()
	fm := &chanMessaging{asWho: map[string]bool{}, items: []*messagingv1.InboxItem{{Id: "old", Channel: "general", Body: "before"}}}
	_, h := messagingv1connect.NewMessagingServiceHandler(fm)
	ms := httptest.NewServer(h)
	t.Cleanup(ms.Close)
	e.srv.Msg = msgclient.NewWith(messagingv1connect.NewMessagingServiceClient(ms.Client(), ms.URL))
	channelPoll = 20 * time.Millisecond
	t.Cleanup(func() { channelPoll = 2 * time.Second })

	vic := signIn(t, e, db, "vic", "viewer", "acme")
	signIn(t, e, db, "olga", "operator", "acme")
	signIn(t, e, db, "bob", "viewer", "beta") // can't see acme
	ch := controlcenterv1connect.NewChannelServiceClient(vic, e.ts.URL+"/api")

	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// The call returns once the first item's response starts, so add it from aside.
	go func() {
		time.Sleep(200 * time.Millisecond)
		fm.add(&messagingv1.InboxItem{Id: "new", Channel: "general", From: "athena@acme", Body: "hi @vic", MentionsMe: true})
	}()
	st, err := ch.WatchChannelItems(sctx, connect.NewRequest(&ccv1.WatchChannelItemsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Receive() {
		t.Fatalf("no item: %v", st.Err())
	}
	got := st.Msg()
	st.Close()
	if got.Workspace != "acme" || got.Item.Id != "new" || !got.Item.MentionsMe {
		t.Fatalf("item %+v", got)
	}
	fm.mu.Lock()
	r0 := fm.reads[0]
	who := fm.asWho["vic@acme"]
	fm.mu.Unlock()
	if !who || r0.GetScope() != "all" || !r0.GetUnreadOnly() || r0.GetMarkRead() {
		t.Fatalf("reads as %v: %+v", fm.asWho, r0)
	}

	// Mentionables: messaging's peers plus the workspace's people.
	ml, err := ch.ListMentionables(ctx, connect.NewRequest(&ccv1.ListMentionablesRequest{Workspace: "acme"}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range ml.Msg.Mentionables {
		names = append(names, fmt.Sprintf("%s:%s:%v", m.Kind, m.Handle, m.Reachable))
	}
	if got := strings.Join(names, " "); got != "agent:athena:true agent:zeus@other:true person:olga:false person:owner:true" {
		t.Fatalf("mentionables %s", got)
	}
	if ml.Msg.Mentionables[0].Role != "Coordinator" || ml.Msg.Mentionables[0].Presence != ccv1.Presence_PRESENCE_WORKING {
		t.Fatalf("athena %+v", ml.Msg.Mentionables[0])
	}
	if _, err := ch.ListMentionables(ctx, connect.NewRequest(&ccv1.ListMentionablesRequest{Workspace: "beta"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("other workspace: %v", err)
	}
}
