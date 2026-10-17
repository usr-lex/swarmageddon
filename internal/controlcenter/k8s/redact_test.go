package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/redact"
)

func TestRedactSecrets(t *testing.T) {
	ctx := context.Background()
	ns := v1alpha1.SystemNamespace
	sec := func(name string, data map[string]string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: map[string][]byte{}}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return s
	}
	stored := sec("ntfy-publisher", map[string]string{"token": "tk_redact_test_1", "username": "swarm-admin", "url": "http://ntfy.swarmageddon-platform.svc"})
	c := RedactSecrets(fake.NewClientBuilder().WithScheme(Scheme()).WithObjects(stored).Build())
	// Read, created, updated and patched values are all registered.
	var got corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "ntfy-publisher"}, &got); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, sec("notify-acme-slack", map[string]string{"url": "https://hooks.slack.com/services/T0/B0/redacttest2"})); err != nil {
		t.Fatal(err)
	}
	db := sec("control-center-db", map[string]string{"url": "postgres://cc@postgres:5432/cc"})
	if err := c.Create(ctx, db); err != nil {
		t.Fatal(err)
	}
	db.Data["url"] = []byte("postgres://cc:pw-redact-test-3@postgres:5432/cc")
	if err := c.Update(ctx, db); err != nil {
		t.Fatal(err)
	}
	patch := client.RawPatch(types.MergePatchType, []byte(`{"data":{"npm":"bnBtX3JlZGFjdF90ZXN0XzQ="}}`)) // npm_redact_test_4
	if err := c.Patch(ctx, sec("ntfy-publisher", nil), patch); err != nil {
		t.Fatal(err)
	}

	out := redact.Default.String("tk_redact_test_1 https://hooks.slack.com/services/T0/B0/redacttest2 pw-redact-test-3 npm_redact_test_4 " +
		"swarm-admin http://ntfy.swarmageddon-platform.svc")
	want := "[redacted] [redacted] [redacted] [redacted] swarm-admin http://ntfy.swarmageddon-platform.svc"
	if out != want {
		t.Fatalf("got  %q\nwant %q", out, want)
	}
}
