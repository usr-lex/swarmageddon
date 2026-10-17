package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1/controlcenterv1connect"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store/storetest"
)

func TestApplyRepoCredentials(t *testing.T) {
	rt := &v1alpha1.RoleTemplate{ObjectMeta: metav1.ObjectMeta{Name: "implementer", Namespace: "ws-acme"}}
	e := newEnv(t, workspace("acme", "owner@example.com"), room("acme", "athena"), rt)
	db, err := store.Open(context.Background(), storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	e.srv.SetDB(db)
	ctx := context.Background()
	u, err := db.SignIn(ctx, store.Identity{Provider: "oidc:kc", Subject: "owner", Handle: "owner", Email: "owner@example.com", MappedRole: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	wsc := controlcenterv1connect.NewWorkspaceServiceClient(e.as(t, session.Data{UserID: u.ID}), e.ts.URL+"/api")

	got, err := wsc.GetWorkspace(ctx, connect.NewRequest(&ccv1.GetWorkspaceRequest{Name: "acme"}))
	if err != nil {
		t.Fatal(err)
	}
	plan := got.Msg.Plan
	plan.Repositories = []*ccv1.RepositoryPlan{{Name: "api", ImportUrl: "https://github.com/acme/api.git", ImportCredential: "bot:ghp_importsecret",
		PushMirror: "https://github.com/acme/api-mirror.git", MirrorToken: "ghp_mirrorsecret"}}
	if _, err := wsc.ApplyWorkspace(ctx, connect.NewRequest(&ccv1.ApplyWorkspaceRequest{Plan: plan})); err != nil {
		t.Fatal(err)
	}
	var w v1alpha1.Workspace
	if err := e.c.Get(ctx, types.NamespacedName{Name: "acme"}, &w); err != nil {
		t.Fatal(err)
	}
	r := w.Spec.Repositories[0]
	creds := map[string]string{}
	for _, name := range []string{r.ImportSecretName, r.MirrorSecretName} {
		var s corev1.Secret
		if err := e.c.Get(ctx, types.NamespacedName{Namespace: "ws-acme", Name: name}, &s); err != nil {
			t.Fatalf("secret %q: %v", name, err)
		}
		creds[name] = string(s.Data["username"]) + ":" + string(s.Data["token"])
	}
	if creds[r.ImportSecretName] != "bot:ghp_importsecret" || creds[r.MirrorSecretName] != ":ghp_mirrorsecret" {
		t.Fatalf("credentials %v", creds)
	}

	// The values come back nowhere: not in the plan, not in the audit log.
	got, err = wsc.GetWorkspace(ctx, connect.NewRequest(&ccv1.GetWorkspaceRequest{Name: "acme"}))
	if err != nil {
		t.Fatal(err)
	}
	if rp := got.Msg.Plan.Repositories[0]; rp.ImportCredential != "" || rp.MirrorToken != "" || rp.MirrorSecretName != r.MirrorSecretName {
		t.Fatalf("plan %+v", rp)
	}
	evs, _ := db.QueryAudit(ctx, store.AuditFilter{})
	n := 0
	for _, ev := range evs {
		if strings.Contains(ev.Before+ev.After+ev.Object, "secret") && strings.Contains(ev.Before+ev.After, "ghp_") {
			t.Fatalf("credential in audit %+v", ev)
		}
		if ev.Action == "workspace.repoCredential" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d credential audit events", n)
	}

	// Re-applying without a credential keeps the Secrets; a new token makes a new one.
	plan = got.Msg.Plan
	if _, err := wsc.ApplyWorkspace(ctx, connect.NewRequest(&ccv1.ApplyWorkspaceRequest{Plan: plan})); err != nil {
		t.Fatal(err)
	}
	plan.Repositories[0].MirrorToken = "ghp_rotated"
	if _, err := wsc.ApplyWorkspace(ctx, connect.NewRequest(&ccv1.ApplyWorkspaceRequest{Plan: plan})); err != nil {
		t.Fatal(err)
	}
	_ = e.c.Get(ctx, types.NamespacedName{Name: "acme"}, &w)
	if r2 := w.Spec.Repositories[0]; r2.ImportSecretName != r.ImportSecretName || r2.MirrorSecretName == r.MirrorSecretName {
		t.Fatalf("after rotation %+v (was %+v)", r2, r)
	}
	var l corev1.SecretList
	_ = e.c.List(ctx, &l, client.InNamespace("ws-acme"), client.MatchingLabels{v1alpha1.LabelComponent: k8s.RepoCredentialComponent})
	if len(l.Items) != 3 {
		t.Fatalf("%d credential secrets", len(l.Items))
	}

	// A credential without its URL is refused.
	plan.Repositories = append(plan.Repositories, &ccv1.RepositoryPlan{Name: "web", MirrorToken: "t"})
	if _, err := wsc.ApplyWorkspace(ctx, connect.NewRequest(&ccv1.ApplyWorkspaceRequest{Plan: plan})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("token without mirror: %v", err)
	}
}
