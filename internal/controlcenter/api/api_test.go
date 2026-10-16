package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1/controlcenterv1connect"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/approvals"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/audit"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/local"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/observatory"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/spend"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store/storetest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	storetest.Stop()
	os.Exit(code)
}

const ns = v1alpha1.SystemNamespace

type env struct {
	srv *Server
	ts  *httptest.Server
	c   client.Client
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	objs = append(objs, &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}})
	c := fake.NewClientBuilder().WithScheme(k8s.Scheme()).WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.Platform{}, &v1alpha1.Workspace{}).Build()
	kc := &k8s.Client{C: c, Kube: kubefake.NewSimpleClientset(), Namespace: ns}
	sessions, _ := session.NewManager(bytes.Repeat([]byte("k"), 32), false)
	log := slog.New(slog.DiscardHandler)
	srv := &Server{K: kc, Sessions: sessions, Setup: auth.NewLimiter(), Logins: auth.NewLimiter(), Inbox: &InboxHub{}, Log: log,
		Spend: &spend.Poller{}}
	srv.Audit = audit.New(log, srv.DB)
	sessions.DB = func() session.DB {
		if db := srv.DB(); db != nil {
			return db
		}
		return nil
	}
	sessions.Promote = srv.Promote
	srv.Approvals = approvals.NewManager(nil, srv.ApprovalLookup())
	srv.Fleet = NewFleet(kc, srv.Approvals)
	srv.Notify = &notify.Dispatcher{Dir: srv.NotifyDirectory(), Log: log}
	srv.Rec = &observatory.Recorder{DB: srv.DB}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{srv: srv, ts: ts, c: c}
}

func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

// as returns an HTTP client signed in with a session for d.
func (e *env) as(t *testing.T, d session.Data) *http.Client {
	t.Helper()
	rec := httptest.NewRecorder()
	if _, err := e.srv.Sessions.Create(context.Background(), rec, httptest.NewRequest("GET", "/", nil), d); err != nil {
		t.Fatal(err)
	}
	hc := e.client()
	u, _ := url.Parse(e.ts.URL)
	hc.Jar.SetCookies(u, rec.Result().Cookies())
	return hc
}

func code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func secret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func TestSetupClaim(t *testing.T) {
	e := newEnv(t, secret("setup-token", map[string]string{"token": "ABCDEF234567"}))
	ctx := context.Background()
	hc := e.client()
	setup := controlcenterv1connect.NewSetupServiceClient(hc, e.ts.URL+"/api")
	authc := controlcenterv1connect.NewAuthServiceClient(hc, e.ts.URL+"/api")

	st, err := setup.GetSetupStatus(ctx, connect.NewRequest(&ccv1.GetSetupStatusRequest{}))
	if err != nil || st.Msg.AdminClaimed || st.Msg.DatabaseReady {
		t.Fatalf("status %+v %v", st, err)
	}
	if _, err := setup.ClaimAdmin(ctx, connect.NewRequest(&ccv1.ClaimAdminRequest{Token: "WRONG"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := setup.ClaimAdmin(ctx, connect.NewRequest(&ccv1.ClaimAdminRequest{Token: "ABCDEF234567", Username: "admin", Password: "short"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("short password: %v", err)
	}
	if _, err := authc.GetMe(ctx, connect.NewRequest(&ccv1.GetMeRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("me before claim: %v", err)
	}
	res, err := setup.ClaimAdmin(ctx, connect.NewRequest(&ccv1.ClaimAdminRequest{Token: " ABCDEF234567\n", Username: "admin", Password: "correct horse battery", Email: "root@example.com"}))
	if err != nil || res.Msg.User.Role != ccv1.Role_ROLE_ADMIN {
		t.Fatalf("claim: %+v %v", res, err)
	}
	if !strings.Contains(res.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(res.Header().Get("Set-Cookie"), "SameSite=Lax") {
		t.Fatalf("session cookie %q", res.Header().Get("Set-Cookie"))
	}
	me, err := authc.GetMe(ctx, connect.NewRequest(&ccv1.GetMeRequest{}))
	if err != nil || me.Msg.User.Handle != "admin" || me.Msg.User.Role != ccv1.Role_ROLE_ADMIN {
		t.Fatalf("me: %+v %v", me, err)
	}
	var p v1alpha1.Platform
	e.c.Get(ctx, types.NamespacedName{Name: "default"}, &p)
	if !p.Status.AdminClaimed {
		t.Fatal("adminClaimed not recorded")
	}
	var la corev1.Secret
	if err := e.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "local-admin"}, &la); err != nil {
		t.Fatal(err)
	}
	if local.Verify(string(la.Data["passwordHash"]), "correct horse battery") != nil || string(la.Data["username"]) != "admin" {
		t.Fatalf("local-admin secret %v", la.Data)
	}
	if _, err := setup.ClaimAdmin(ctx, connect.NewRequest(&ccv1.ClaimAdminRequest{Token: "ABCDEF234567"})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("second claim: %v", err)
	}
	// Every attempt is audited (to the log and, before the database, memory).
	var claims int
	for _, ev := range e.srv.Audit.Pending() {
		if ev.Action == "setup.claim" {
			claims++
		}
	}
	if claims != 3 { // wrong token, ok, already claimed (the invalid password is refused before)
		t.Fatalf("audited %d claim attempts", claims)
	}

	// Local login and logout.
	hc2 := e.client()
	a2 := controlcenterv1connect.NewAuthServiceClient(hc2, e.ts.URL+"/api")
	if _, err := a2.LocalLogin(ctx, connect.NewRequest(&ccv1.LocalLoginRequest{Username: "admin", Password: "nope"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("bad login: %v", err)
	}
	if _, err := a2.LocalLogin(ctx, connect.NewRequest(&ccv1.LocalLoginRequest{Username: "admin", Password: "correct horse battery"})); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := a2.GetMe(ctx, connect.NewRequest(&ccv1.GetMeRequest{})); err != nil {
		t.Fatalf("me after login: %v", err)
	}
	if _, err := a2.Logout(ctx, connect.NewRequest(&ccv1.LogoutRequest{})); err != nil {
		t.Fatal(err)
	}
	if _, err := a2.GetMe(ctx, connect.NewRequest(&ccv1.GetMeRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("me after logout: %v", err)
	}
	prov, _ := a2.ListProviders(ctx, connect.NewRequest(&ccv1.ListProvidersRequest{}))
	if len(prov.Msg.Providers) != 1 || prov.Msg.Providers[0].Kind != ccv1.ProviderKind_PROVIDER_KIND_LOCAL || !prov.Msg.AdminClaimed {
		t.Fatalf("providers %+v", prov.Msg)
	}
}

func TestSetupRateLimit(t *testing.T) {
	e := newEnv(t, secret("setup-token", map[string]string{"token": "RIGHT"}))
	ctx := context.Background()
	setup := controlcenterv1connect.NewSetupServiceClient(e.client(), e.ts.URL+"/api")
	for i := 0; i < 5; i++ {
		if _, err := setup.ClaimAdmin(ctx, connect.NewRequest(&ccv1.ClaimAdminRequest{Token: "guess"})); code(err) != connect.CodePermissionDenied {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	// Even the right token is refused while locked out.
	if _, err := setup.ClaimAdmin(ctx, connect.NewRequest(&ccv1.ClaimAdminRequest{Token: "RIGHT"})); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("locked out: %v", err)
	}
}

func post(t *testing.T, e *env, path string, h map[string]string) int {
	req, _ := http.NewRequest("POST", e.ts.URL+path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestCSRFAndStatic(t *testing.T) {
	e := newEnv(t)
	path := "/api/swarm.controlcenter.v1.AuthService/ListProviders"
	if c := post(t, e, path, nil); c != http.StatusForbidden {
		t.Fatalf("no Connect header: %d", c)
	}
	if c := post(t, e, path, map[string]string{"Connect-Protocol-Version": "1", "Origin": "https://evil.example"}); c != http.StatusForbidden {
		t.Fatalf("cross origin: %d", c)
	}
	if c := post(t, e, path, map[string]string{"Connect-Protocol-Version": "1", "Origin": e.ts.URL}); c != http.StatusOK {
		t.Fatalf("same origin: %d", c)
	}
	for _, p := range []string{"/", "/workspaces/acme/rooms/athena", "/healthz"} {
		resp, err := http.Get(e.ts.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", p, resp, err)
		}
		resp.Body.Close()
	}
}

func workspace(name string, owners ...string) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:   v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: name}, Owners: owners},
		Status: v1alpha1.WorkspaceStatus{Namespace: "ws-" + name}}
}

func room(ws, name string) *v1alpha1.Room {
	return &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ws-" + ws},
		Spec: v1alpha1.RoomSpec{Role: "implementer", CLI: "claude"}}
}

func TestRBAC(t *testing.T) {
	e := newEnv(t, workspace("acme", "owner@example.com"), workspace("beta"), room("acme", "athena"), room("beta", "hermes"))
	db, err := store.Open(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e.srv.SetDB(db)
	ctx := context.Background()
	e.srv.Fleet.Refresh(ctx)

	user := func(handle, role string, assigned ...string) *http.Client {
		u, err := db.SignIn(ctx, store.Identity{Provider: "oidc:kc", Subject: handle, Handle: handle, Email: handle + "@example.com", MappedRole: role})
		if err != nil {
			t.Fatal(err)
		}
		db.SetAssignments(ctx, u.ID, assigned)
		return e.as(t, session.Data{UserID: u.ID})
	}
	admin := user("root", "admin")
	owner := user("owner", "operator")
	viewer := user("vic", "viewer", "acme")
	outsider := user("out", "operator", "beta")

	rooms := func(hc *http.Client) controlcenterv1connect.RoomServiceClient {
		return controlcenterv1connect.NewRoomServiceClient(hc, e.ts.URL+"/api")
	}
	wsc := func(hc *http.Client) controlcenterv1connect.WorkspaceServiceClient {
		return controlcenterv1connect.NewWorkspaceServiceClient(hc, e.ts.URL+"/api")
	}
	list := func(hc *http.Client) string {
		r, err := wsc(hc).ListWorkspaces(ctx, connect.NewRequest(&ccv1.ListWorkspacesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, w := range r.Msg.Workspaces {
			names = append(names, w.Name)
		}
		return strings.Join(names, ",")
	}
	for who, want := range map[string]string{"admin": "acme,beta", "owner": "acme", "viewer": "acme", "outsider": "beta"} {
		hc := map[string]*http.Client{"admin": admin, "owner": owner, "viewer": viewer, "outsider": outsider}[who]
		if got := list(hc); got != want {
			t.Errorf("%s sees %q, want %q", who, got, want)
		}
	}
	pause := func(hc *http.Client) error {
		_, err := rooms(hc).PauseRoom(ctx, connect.NewRequest(&ccv1.PauseRoomRequest{Room: &ccv1.RoomRef{Workspace: "acme", Name: "athena"}}))
		return err
	}
	if code(pause(viewer)) != connect.CodePermissionDenied || code(pause(outsider)) != connect.CodePermissionDenied {
		t.Fatal("viewer or outsider paused a room")
	}
	if err := pause(owner); err != nil {
		t.Fatalf("owner pause: %v", err)
	}
	var r v1alpha1.Room
	e.c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "athena"}, &r)
	if !r.Spec.Paused {
		t.Fatal("room not paused")
	}
	if _, err := rooms(viewer).GetRoom(ctx, connect.NewRequest(&ccv1.GetRoomRequest{Room: &ccv1.RoomRef{Workspace: "acme", Name: "athena"}})); err != nil {
		t.Fatalf("viewer get room: %v", err)
	}
	if _, err := rooms(outsider).GetRoom(ctx, connect.NewRequest(&ccv1.GetRoomRequest{Room: &ccv1.RoomRef{Workspace: "acme", Name: "athena"}})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("outsider get room: %v", err)
	}
	_, err = rooms(owner).RemoveRoom(ctx, connect.NewRequest(&ccv1.RemoveRoomRequest{Room: &ccv1.RoomRef{Workspace: "acme", Name: "athena"}, ConfirmName: "nope"}))
	if code(err) != connect.CodeInvalidArgument {
		t.Fatalf("remove without confirmation: %v", err)
	}

	// Platform settings are admin only.
	pc := func(hc *http.Client) error {
		_, err := controlcenterv1connect.NewPlatformServiceClient(hc, e.ts.URL+"/api").UpdateHostname(ctx,
			connect.NewRequest(&ccv1.UpdateHostnameRequest{Hostname: &ccv1.HostnameSettings{Hostname: "agents.example.com", TlsMode: "selfSigned"}}))
		return err
	}
	if code(pc(owner)) != connect.CodePermissionDenied {
		t.Fatal("operator changed the hostname")
	}
	if err := pc(admin); err != nil {
		t.Fatalf("admin hostname: %v", err)
	}
	// Users are admin only.
	if _, err := controlcenterv1connect.NewUserServiceClient(viewer, e.ts.URL+"/api").ListUsers(ctx, connect.NewRequest(&ccv1.ListUsersRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer listed users: %v", err)
	}

	// An operator creating a workspace becomes its owner; viewers can't create.
	plan := &ccv1.WorkspacePlan{Name: "gamma", Org: "gamma", Internet: true,
		Agents: []*ccv1.AgentPlan{{Name: "metis", Role: "implementer", Cli: "ore"}}}
	if _, err := wsc(viewer).ApplyWorkspace(ctx, connect.NewRequest(&ccv1.ApplyWorkspaceRequest{Plan: plan})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer created a workspace: %v", err)
	}
	pv, err := wsc(outsider).PreviewWorkspace(ctx, connect.NewRequest(&ccv1.PreviewWorkspaceRequest{Plan: plan}))
	if err != nil || len(pv.Msg.Problems) != 0 || pv.Msg.Changes[0].Action != ccv1.ChangeAction_CHANGE_ACTION_CREATE {
		t.Fatalf("preview %+v %v", pv, err)
	}
	// Names used in another workspace are refused.
	dup := &ccv1.WorkspacePlan{Name: "delta", Org: "delta", Agents: []*ccv1.AgentPlan{{Name: "athena", Role: "implementer", Cli: "claude"}}}
	if pv, _ := wsc(outsider).PreviewWorkspace(ctx, connect.NewRequest(&ccv1.PreviewWorkspaceRequest{Plan: dup})); len(pv.Msg.Problems) == 0 {
		t.Fatal("duplicate agent name accepted")
	}

	// Every action is audited.
	evs, _ := db.QueryAudit(ctx, store.AuditFilter{Actions: []string{"room.pause", "platform.hostname"}})
	if len(evs) != 2 {
		t.Fatalf("audit %+v", evs)
	}
	for _, ev := range evs {
		if ev.Action == "room.pause" && (ev.Actor != "owner@example.com" || ev.Workspace != "acme" || !strings.Contains(ev.After, `"paused":true`)) {
			t.Fatalf("pause audit %+v", ev)
		}
	}
}

func TestStatelessSessionMovesToDB(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hc := e.as(t, session.Data{Provider: "local", Subject: "admin", Handle: "admin", Role: "admin"})
	authc := controlcenterv1connect.NewAuthServiceClient(hc, e.ts.URL+"/api")
	if me, err := authc.GetMe(ctx, connect.NewRequest(&ccv1.GetMeRequest{})); err != nil || me.Msg.User.Id != "" {
		t.Fatalf("stateless me: %+v %v", me, err)
	}
	db, err := store.Open(ctx, storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e.srv.SetDB(db)
	me, err := authc.GetMe(ctx, connect.NewRequest(&ccv1.GetMeRequest{}))
	if err != nil || me.Msg.User.Id == "" || me.Msg.User.Role != ccv1.Role_ROLE_ADMIN {
		t.Fatalf("after DB: %+v %v", me, err)
	}
	// Bootstrap audit events are copied in.
	e.srv.Audit.Record(ctx, store.AuditEvent{Actor: "x", Action: "test.event"})
	if err := e.srv.Audit.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	evs, _ := db.QueryAudit(ctx, store.AuditFilter{Actions: []string{"test."}})
	if len(evs) != 1 {
		t.Fatalf("flushed %d", len(evs))
	}
}
