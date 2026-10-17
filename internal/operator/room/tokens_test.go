package room

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/messaging/tokenhash"
)

// TestTokenHashes: the workspace's messaging-token-hashes holds the digests
// of every room's issued tokens and never a token; it follows rotation and
// removal.
func TestTokenHashes(t *testing.T) {
	e := newEnv(t)
	e.addRoom("athena", nil)
	e.addRoom("hermes", nil)
	e.reconcile("athena", "hermes")
	ctx := context.Background()
	hashes := func() (string, tokenhash.Set) {
		t.Helper()
		var cm corev1.ConfigMap
		must(t, e.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: tokenhash.ConfigMapName}, &cm))
		return cm.Data[tokenhash.DataKey], tokenhash.Parse(cm.Data[tokenhash.DataKey])
	}
	// The fakes' tokens are shorter than real ones, so look them up directly.
	listed := func(set tokenhash.Set, tok string) bool {
		_, ok := set[sha256.Sum256([]byte(tok))]
		return ok
	}
	tokens := func(room string) []string {
		sec := e.secret(room)
		var out []string
		for _, k := range []string{KeyForgeToken, KeyGatewayKey, KeyEgressAuth} {
			if len(sec.Data[k]) == 0 {
				t.Fatalf("%s has no %s", room, k)
			}
			out = append(out, string(sec.Data[k]))
		}
		return out
	}

	raw, set := hashes()
	for _, room := range []string{"athena", "hermes"} {
		for _, tok := range tokens(room) {
			if strings.Contains(raw, tok) {
				t.Fatalf("a token of %s is in the ConfigMap", room)
			}
			if !listed(set, tok) {
				t.Errorf("a token of %s is missing", room)
			}
		}
	}
	// Real egress credentials are long enough to be found in a message.
	if !set.Contains("proxy password: " + tokens("athena")[2]) {
		t.Error("egress credential not found in a body")
	}

	old := tokens("athena")
	e.update("athena", func(r *v1.Room) { r.Annotations = map[string]string{v1.AnnotationRotate: "now"} })
	e.reconcile("athena")
	_, set = hashes()
	for _, tok := range old {
		if listed(set, tok) {
			t.Error("a rotated-out token is still listed")
		}
	}
	for _, tok := range tokens("athena") {
		if !listed(set, tok) {
			t.Error("a new token is missing")
		}
	}

	hermes := tokens("hermes")
	must(t, e.c.Delete(ctx, e.room("hermes")))
	e.reconcile("hermes")
	_, set = hashes()
	for _, tok := range hermes {
		if listed(set, tok) {
			t.Error("a removed room's token is still listed")
		}
	}
	if !listed(set, tokens("athena")[0]) {
		t.Error("removing hermes dropped athena's tokens")
	}
}
