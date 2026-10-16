package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1/controlcenterv1connect"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store/storetest"
)

func TestWardenAndQuarantine(t *testing.T) {
	e := newEnv(t, workspace("acme", "owner@example.com"), room("acme", "athena"))
	e.srv.Verdicts = &VerdictHub{}
	db, err := store.Open(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e.srv.SetDB(db)
	ctx := context.Background()
	user := func(handle, role string, assigned ...string) *session.Data {
		u, err := db.SignIn(ctx, store.Identity{Provider: "oidc:kc", Subject: handle, Handle: handle, Email: handle + "@example.com", MappedRole: role})
		if err != nil {
			t.Fatal(err)
		}
		db.SetAssignments(ctx, u.ID, assigned)
		return &session.Data{UserID: u.ID}
	}
	owner := e.as(t, *user("owner", "operator"))
	viewer := e.as(t, *user("vic", "viewer", "acme"))

	// A verdict arrives from the messaging audit.
	e.srv.OnVerdict(ctx, "acme", &messagingv1.Verdict{Id: "v1", Room: "athena@acme", Layer: "gateway", Severity: messagingv1.Severity_SEVERITY_HIGH,
		Reason: "force push to main", Action: "quarantined", Source: "gateway", At: timestamppb.New(time.Now())})
	e.srv.OnVerdict(ctx, "acme", &messagingv1.Verdict{Id: "v1", Room: "athena@acme", Severity: messagingv1.Severity_SEVERITY_HIGH}) // replay

	vw := controlcenterv1connect.NewWardenServiceClient(viewer, e.ts.URL+"/api")
	list, err := vw.ListVerdicts(ctx, connect.NewRequest(&ccv1.ListVerdictsRequest{Workspace: "acme", UndecidedOnly: true}))
	if err != nil || len(list.Msg.Verdicts) != 1 || list.Msg.Verdicts[0].Severity != ccv1.Severity_SEVERITY_HIGH {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := vw.DecideVerdict(ctx, connect.NewRequest(&ccv1.DecideVerdictRequest{Id: "v1", Decision: "dismiss"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer decided: %v", err)
	}
	ow := controlcenterv1connect.NewWardenServiceClient(owner, e.ts.URL+"/api")
	d, err := ow.DecideVerdict(ctx, connect.NewRequest(&ccv1.DecideVerdictRequest{Id: "v1", Decision: "confirm", Note: "it really did"}))
	if err != nil || d.Msg.Verdict.Decision != "confirm" || d.Msg.Verdict.DecidedBy != "owner@example.com" {
		t.Fatalf("decide %+v %v", d, err)
	}

	// Quarantine and release: operators only, audited, reason shown.
	vr := controlcenterv1connect.NewRoomServiceClient(viewer, e.ts.URL+"/api")
	or := controlcenterv1connect.NewRoomServiceClient(owner, e.ts.URL+"/api")
	ref := &ccv1.RoomRef{Workspace: "acme", Name: "athena"}
	if _, err := or.QuarantineRoom(ctx, connect.NewRequest(&ccv1.QuarantineRoomRequest{Room: ref, Reason: "warden: force push"})); err != nil {
		t.Fatal(err)
	}
	e.srv.Fleet.Refresh(ctx)
	rooms, _ := vr.ListRooms(ctx, connect.NewRequest(&ccv1.ListRoomsRequest{Workspace: "acme"}))
	if len(rooms.Msg.Rooms) != 1 || !rooms.Msg.Rooms[0].Quarantined || rooms.Msg.Rooms[0].QuarantineReason != "warden: force push" {
		t.Fatalf("fleet %+v", rooms.Msg.Rooms)
	}
	if _, err := vr.ReleaseRoom(ctx, connect.NewRequest(&ccv1.ReleaseRoomRequest{Room: ref})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer released: %v", err)
	}
	if _, err := or.ReleaseRoom(ctx, connect.NewRequest(&ccv1.ReleaseRoomRequest{Room: ref, Note: "checked"})); err != nil {
		t.Fatal(err)
	}
	var r v1alpha1.Room
	e.c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "athena"}, &r)
	if r.Spec.Quarantined || r.Annotations["swarmageddon.dev/released-by"] != "owner@example.com" {
		t.Fatalf("after release %+v", r)
	}
	if _, err := or.ReleaseRoom(ctx, connect.NewRequest(&ccv1.ReleaseRoomRequest{Room: ref})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("release twice: %v", err)
	}
	evs, _ := db.QueryAudit(ctx, store.AuditFilter{Actions: []string{"room.release", "room.quarantine", "verdict."}})
	var acts []string
	for _, ev := range evs {
		acts = append(acts, ev.Action)
	}
	if got := strings.Join(acts, ","); !strings.Contains(got, "room.release") || !strings.Contains(got, "room.quarantine") || !strings.Contains(got, "verdict.confirm") {
		t.Fatalf("audit %s", got)
	}
}
