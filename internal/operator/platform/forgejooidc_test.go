package platform

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

// fakeExec plays the Forgejo container: it answers auth list from its
// sources and records the commands and stdin it was given.
type fakeExec struct {
	list  string
	fail  error
	calls [][]string
	stdin []string
}

func (f *fakeExec) Exec(_ context.Context, ns, pod, container string, cmd []string, stdin io.Reader, stdout, _ io.Writer) error {
	if ns != v1alpha1.PlatformNamespace || pod != "forgejo-0" || container != "forgejo" {
		return errors.New("wrong container")
	}
	f.calls = append(f.calls, cmd)
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.stdin = append(f.stdin, in)
	if f.fail != nil {
		return f.fail
	}
	if slices.Equal(cmd, []string{"forgejo", "admin", "auth", "list"}) {
		_, _ = io.WriteString(stdout, "ID\tName\tType\tEnabled\n"+f.list)
	}
	return nil
}

func TestAuthSourceID(t *testing.T) {
	list := "ID\tName\tType\tEnabled\n1\tcorp ldap\tLDAP (via BindDN)\ttrue\n3\t\tkeycloak\t\tOAuth2\ttrue\n"
	if id, ok := authSourceID(list, "keycloak"); !ok || id != 3 {
		t.Errorf("keycloak = %d, %v", id, ok)
	}
	if _, ok := authSourceID(list, "corp"); ok {
		t.Error("matched a prefix")
	}
	if _, ok := authSourceID("ID\tName\tType\tEnabled\n", "keycloak"); ok {
		t.Error("found in an empty list")
	}
}

func TestForgejoOIDC(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	for _, ns := range []string{v1alpha1.SystemNamespace, v1alpha1.PlatformNamespace} {
		if err := env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.Client.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.SystemNamespace, Name: "oidc-keycloak"},
		Data: map[string][]byte{"clientSecret": []byte("s3cret")}}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.PlatformNamespace, Name: "forgejo-0"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "forgejo", Image: "forgejo"}}}}
	if err := env.Client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := env.Client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}

	fx := &fakeExec{}
	r := &Reconciler{Client: env.OperatorClient(t), Scheme: env.Scheme, Exec: fx}
	p := &v1alpha1.Platform{Spec: v1alpha1.PlatformSpec{
		Hostname: "agents.example.com",
		Auth: v1alpha1.AuthSpec{
			OIDC: []v1alpha1.OIDCProvider{{Name: "keycloak", Issuer: "https://kc.example.com/realms/acme/", ClientID: "swarm",
				ClientSecretRef: corev1.LocalObjectReference{Name: "oidc-keycloak"}, Scopes: []string{"groups", "email"}}},
			RoleMappings: []v1alpha1.RoleMapping{{Group: "devs", Role: v1alpha1.RoleOperator}, {Group: "platform-admins", Role: v1alpha1.RoleAdmin}},
		},
		Services: v1alpha1.ServicesSpec{Forgejo: v1alpha1.ForgejoService{OIDCLogin: true}},
	}}
	setCond(p, v1alpha1.ConditionForgejoReady, true, "Ready", "")
	cond := func() *metav1.Condition { return meta.FindStatusCondition(p.Status.Conditions, ConditionForgejoOIDC) }

	r.forgejoOIDC(ctx, p)
	if c := cond(); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v", c)
	}
	if !strings.Contains(cond().Message, "https://forge.agents.example.com/user/oauth2/keycloak/callback") {
		t.Errorf("message doesn't name the redirect URI: %s", cond().Message)
	}
	if len(fx.calls) != 2 {
		t.Fatalf("calls = %q", fx.calls)
	}
	add := fx.calls[1]
	want := []string{"sh", "-c", `exec forgejo admin auth "$@" --secret "$(cat)"`, "sh", "add-oauth",
		"--name", "keycloak", "--provider", "openidConnect", "--key", "swarm",
		"--auto-discover-url", "https://kc.example.com/realms/acme/.well-known/openid-configuration",
		"--group-claim-name", "groups", "--admin-group", "platform-admins",
		"--scopes", "openid", "--scopes", "profile", "--scopes", "email", "--scopes", "groups"}
	if !slices.Equal(add, want) {
		t.Errorf("add-oauth =\n%q\nwant\n%q", add, want)
	}
	if fx.stdin[1] != "s3cret" || slices.ContainsFunc(add, func(a string) bool { return strings.Contains(a, "s3cret") }) {
		t.Error("the secret must go in on stdin only")
	}

	// Nothing changed: nothing runs.
	fx.calls = nil
	r.forgejoOIDC(ctx, p)
	if len(fx.calls) != 0 {
		t.Fatalf("ran again: %q", fx.calls)
	}

	// A change updates the existing source.
	p.Spec.Auth.OIDC[0].ClientID = "swarm2"
	fx.list = "7\tkeycloak\tOAuth2\ttrue\n"
	r.forgejoOIDC(ctx, p)
	if len(fx.calls) != 2 || !slices.Equal(fx.calls[1][4:7], []string{"update-oauth", "--id", "7"}) {
		t.Fatalf("calls = %q", fx.calls)
	}

	// A failing exec is reported and retried; the annotation stays old.
	p.Spec.Auth.OIDC[0].ClientID = "swarm3"
	fx.calls, fx.fail = nil, errors.New("container not running")
	if d := r.forgejoOIDC(ctx, p); d == 0 {
		t.Error("no retry after a failure")
	}
	if c := cond(); c.Status != metav1.ConditionFalse || c.Reason != "ExecFailed" {
		t.Errorf("condition = %+v", c)
	}
	fx.fail = nil
	r.forgejoOIDC(ctx, p)
	if len(fx.calls) != 3 {
		t.Errorf("not retried: %q", fx.calls)
	}

	// The hash is on the pod, so a new pod gets the source written again.
	var cur corev1.Pod
	if err := env.Client.Get(ctx, types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "forgejo-0"}, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Annotations[annForgejoOIDC] == "" {
		t.Fatal("pod not annotated")
	}

	p.Spec.Auth.OIDC = nil
	r.forgejoOIDC(ctx, p)
	if c := cond(); c.Status != metav1.ConditionFalse || c.Reason != "NoProvider" {
		t.Errorf("condition = %+v", c)
	}
	p.Spec.Services.Forgejo.OIDCLogin = false
	r.forgejoOIDC(ctx, p)
	if cond() != nil {
		t.Error("condition kept with oidcLogin off")
	}
}
