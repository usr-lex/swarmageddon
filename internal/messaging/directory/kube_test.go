package directory

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/tokenhash"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/testenv"
)

// TestKubeTokenHashes runs the directory with exactly the ClusterRole the
// operator gives messaging, and follows a workspace's token digests.
func TestKubeTokenHashes(t *testing.T) {
	env := testenv.Start(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	objs, err := manifests.Render(manifests.Messaging, manifests.Values{Namespace: v1alpha1.PlatformNamespace, Images: images.Defaults("v0.1.0"),
		Messaging: manifests.MessagingValues{Replicas: 1, Storage: "1Gi", Routes: "nats://messaging-0:6222"}})
	must(err)
	for _, o := range objs {
		if o.GetKind() == "ClusterRole" {
			o.SetResourceVersion("")
			must(env.Client.Create(ctx, o))
		}
	}
	user := "system:serviceaccount:" + v1alpha1.PlatformNamespace + ":messaging"
	must(env.Client.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "messaging-test"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "swarmageddon-messaging"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}},
	}))
	cfg := rest.CopyConfig(env.Config)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:serviceaccounts", "system:authenticated"}}

	must(env.Client.Create(ctx, &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "acme"}, Spec: v1alpha1.WorkspaceSpec{Forge: v1alpha1.ForgeSpec{Org: "acme"}}}))
	ns := v1alpha1.WorkspaceNamespace("acme")
	must(env.Client.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	must(env.Client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "unrelated"}, Data: map[string]string{"a": "b"}}))
	const tok1, tok2 = "0123456789abcdef0123456789abcdef01234567", "sk-AbCdEfGhIjKlMnOpQrStUv"
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: tokenhash.ConfigMapName},
		Data: map[string]string{tokenhash.DataKey: tokenhash.Format([]string{tok1})}}
	must(env.Client.Create(ctx, cm))

	k, err := NewKube(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(err)
	go func() { _ = k.Start(ctx) }()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitFor("directory ready", k.Ready)
	has := func(tok string) bool {
		ws, ok := k.Workspace("acme")
		return ok && ws.TokenHashes.Contains("x "+tok+" y")
	}
	waitFor("first token", func() bool { return has(tok1) })

	cm.Data[tokenhash.DataKey] = tokenhash.Format([]string{tok2})
	must(env.Client.Update(ctx, cm))
	waitFor("rotated token", func() bool { return has(tok2) && !has(tok1) })
}
