package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestPrintWelcome(t *testing.T) {
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p).WithStatusSubresource(p).Build()

	var out bytes.Buffer
	if done, err := printWelcome(ctx, c, &out); err != nil || done {
		t.Fatalf("no URL yet: done=%v err=%v", done, err)
	}
	p.Status.URL, p.Status.CAFingerprint = "https://10.0.0.5/", "AB:CD"
	if err := c.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	if done, _ := printWelcome(ctx, c, &out); done {
		t.Fatal("printed before the setup token existed")
	}
	if err := c.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: v1alpha1.SystemNamespace, Name: "setup-token"},
		Data: map[string][]byte{"token": []byte("TOKEN123")}}); err != nil {
		t.Fatal(err)
	}
	done, err := printWelcome(ctx, c, &out)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	for _, want := range []string{"https://10.0.0.5/", "AB:CD", "TOKEN123", "port-forward"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
