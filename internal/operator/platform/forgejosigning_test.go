package platform

import (
	"bytes"
	"context"
	"testing"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Forgejo signs merges with an SSH key of its own (§9.3); an install from
// before the key existed gets one added, and keeps its secret key.
func TestForgejoSigningKey(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	key := types.NamespacedName{Namespace: v1alpha1.PlatformNamespace, Name: "forgejo-secrets"}
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: map[string][]byte{"secretKey": []byte("kept")}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(old).Build()
	r := &Reconciler{Client: c, Scheme: s}
	ctx := context.Background()

	if err := r.ensureForgejoSecrets(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.Data["secretKey"]) != "kept" {
		t.Errorf("secretKey changed: %q", got.Data["secretKey"])
	}
	signer, err := ssh.ParsePrivateKey(got.Data["signingKey"])
	if err != nil {
		t.Fatalf("signingKey: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(got.Data["signingKey.pub"])
	if err != nil {
		t.Fatalf("signingKey.pub: %v", err)
	}
	if !bytes.Equal(pub.Marshal(), signer.PublicKey().Marshal()) {
		t.Error("signingKey.pub isn't signingKey's public key")
	}

	// Stable afterwards.
	if err := r.ensureForgejoSecrets(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var again corev1.Secret
	_ = c.Get(ctx, key, &again)
	if !bytes.Equal(again.Data["signingKey"], got.Data["signingKey"]) {
		t.Error("the signing key changed")
	}
}
