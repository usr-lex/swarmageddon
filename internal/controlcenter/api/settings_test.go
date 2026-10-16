package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1/controlcenterv1connect"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store/storetest"
)

func TestRoomSettingsAndSecrets(t *testing.T) {
	ws := workspace("acme", "owner@example.com")
	ws.Spec.RoomDefaults = v1alpha1.RoomDefaults{Env: map[string]string{"LANG": "C"}}
	ws.Spec.SharedVolumes = []v1alpha1.SharedVolume{{Name: "data", Mounts: []v1alpha1.VolumeMount{{Select: "role:implementer", ReadOnly: true}}}}
	rt := &v1alpha1.RoleTemplate{ObjectMeta: metav1.ObjectMeta{Name: "implementer", Namespace: "ws-acme"},
		Spec: v1alpha1.RoleTemplateSpec{RoomDefaults: v1alpha1.RoomDefaults{Network: &v1alpha1.NetworkAccess{Mode: v1alpha1.NetworkLimited}}}}
	rs := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "room-secrets", Namespace: "ws-acme"}}
	e := newEnv(t, ws, rt, rs, room("acme", "athena"))
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
	ref := &ccv1.RoomRef{Workspace: "acme", Name: "athena"}

	vr := controlcenterv1connect.NewRoomServiceClient(viewer, e.ts.URL+"/api")
	got, err := vr.GetRoomSettings(ctx, connect.NewRequest(&ccv1.GetRoomSettingsRequest{Room: ref}))
	if err != nil {
		t.Fatal(err)
	}
	eff := got.Msg.Effective.Settings
	if eff.Env["LANG"] != "C" || eff.Network.Mode != "limited" || got.Msg.SharedVolumes["data"] != "ro" {
		t.Fatalf("settings %+v", got.Msg)
	}
	upd := &ccv1.RoomSettingsPlan{Env: map[string]string{"LANG": "en"}, SecretEnv: []*ccv1.SecretEnvPlan{{Name: "GH", Secret: &ccv1.SecretKeyPlan{Name: "room-secrets", Key: "gh"}}}}
	if _, err := vr.UpdateRoomSettings(ctx, connect.NewRequest(&ccv1.UpdateRoomSettingsRequest{Room: ref, Settings: upd})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer updated: %v", err)
	}
	or := controlcenterv1connect.NewRoomServiceClient(owner, e.ts.URL+"/api")
	bad := &ccv1.RoomSettingsPlan{Env: map[string]string{"KEY": "sk-abcdefghijklmnopqrst"}}
	if _, err := or.UpdateRoomSettings(ctx, connect.NewRequest(&ccv1.UpdateRoomSettingsRequest{Room: ref, Settings: bad})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("secret-looking env accepted: %v", err)
	}
	res, err := or.UpdateRoomSettings(ctx, connect.NewRequest(&ccv1.UpdateRoomSettingsRequest{Room: ref, Settings: upd}))
	if err != nil || res.Msg.Effective.Settings.Env["LANG"] != "en" || len(res.Msg.Effective.Settings.SecretEnv) != 1 {
		t.Fatalf("update %+v %v", res, err)
	}

	// room-secrets: operators write keys; values never come back.
	vw := controlcenterv1connect.NewWorkspaceServiceClient(viewer, e.ts.URL+"/api")
	if _, err := vw.SetRoomSecret(ctx, connect.NewRequest(&ccv1.SetRoomSecretRequest{Workspace: "acme", Key: "gh", Value: "x"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer set secret: %v", err)
	}
	ow := controlcenterv1connect.NewWorkspaceServiceClient(owner, e.ts.URL+"/api")
	set, err := ow.SetRoomSecret(ctx, connect.NewRequest(&ccv1.SetRoomSecretRequest{Workspace: "acme", Key: "gh", Value: "ghp_secret"}))
	if err != nil || set.Msg.Ref.Name != "room-secrets" {
		t.Fatalf("set %+v %v", set, err)
	}
	keys, err := ow.ListRoomSecrets(ctx, connect.NewRequest(&ccv1.ListRoomSecretsRequest{Workspace: "acme"}))
	if err != nil || len(keys.Msg.Keys) != 1 || keys.Msg.Keys[0] != "gh" {
		t.Fatalf("keys %+v %v", keys, err)
	}
	if _, err := ow.DeleteRoomSecret(ctx, connect.NewRequest(&ccv1.DeleteRoomSecretRequest{Workspace: "acme", Key: "gh"})); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	e.srv.K.C.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: "room-secrets"}, &s)
	if len(s.Data) != 0 {
		t.Fatalf("secret data after delete %v", s.Data)
	}
}
