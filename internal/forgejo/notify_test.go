package forgejo

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

func TestEnsureOrgHookCreatesThenPatches(t *testing.T) {
	f, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /orgs/acme/hooks?limit=50": jsonResp(200, []Hook{}),
		"POST /orgs/acme/hooks":         jsonResp(201, Hook{ID: 3}),
	})
	ctx := context.Background()
	if _, err := c.EnsureOrgHook(ctx, "acme", "https://cc/hooks/forgejo/acme", "s3cret", ForgeNotificationEvents); err != nil {
		t.Fatal(err)
	}
	b := f.mustFind("POST", "/orgs/acme/hooks").Body
	if b["type"] != "forgejo" || b["active"] != true || len(b["events"].([]any)) != len(ForgeNotificationEvents) {
		t.Errorf("body = %v", b)
	}
	if !reflect.DeepEqual(b["config"], map[string]any{"url": "https://cc/hooks/forgejo/acme", "content_type": "json", "secret": "s3cret"}) {
		t.Errorf("config = %v", b["config"])
	}

	f2, c2 := newFake(t, map[string]func(http.ResponseWriter){
		"GET /orgs/acme/hooks?limit=50": jsonResp(200, []Hook{{ID: 3, Config: map[string]string{"url": "https://cc/hooks/forgejo/acme"}}}),
		"PATCH /orgs/acme/hooks/3":      jsonResp(200, Hook{ID: 3}),
	})
	if _, err := c2.EnsureOrgHook(ctx, "acme", "https://cc/hooks/forgejo/acme", "new", ForgeNotificationEvents); err != nil {
		t.Fatal(err)
	}
	if f2.find("POST", "/orgs/acme/hooks") != nil {
		t.Error("created a second hook")
	}
	if p := f2.mustFind("PATCH", "/orgs/acme/hooks/3").Body; p["config"].(map[string]any)["secret"] != "new" {
		t.Errorf("patch = %v", p)
	}
}

func TestRepoPermission(t *testing.T) {
	_, c := newFake(t, map[string]func(http.ResponseWriter){
		"GET /repos/acme/api/collaborators/athena/permission": jsonResp(200, RepoPermission{Permission: "write"}),
		"GET /repos/acme/api/collaborators/metis/permission":  jsonResp(200, RepoPermission{Permission: "none"}),
	})
	p, err := c.GetRepoPermission(context.Background(), "acme", "api", "athena")
	if err != nil || !p.CanRead() {
		t.Fatal(p, err)
	}
	p, err = c.GetRepoPermission(context.Background(), "acme", "api", "metis")
	if err != nil || p.CanRead() {
		t.Fatal(p, err)
	}
}

func TestGPGSignVerifies(t *testing.T) {
	kp, err := NewGPGKey("athena", "athena@agents.acme.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if len(kp.KeyID) != 16 || !strings.Contains(kp.ArmoredPublic, "PUBLIC KEY") {
		t.Fatalf("key = %+v", kp)
	}
	sig, err := SignToken(kp.ArmoredPrivate, "tok123")
	if err != nil {
		t.Fatal(err)
	}
	ring, _ := openpgp.ReadArmoredKeyRing(strings.NewReader(kp.ArmoredPublic))
	if _, err := openpgp.CheckArmoredDetachedSignature(ring, strings.NewReader("tok123"), strings.NewReader(sig), nil); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	again, _, err := ParseGPGKey(kp.ArmoredPrivate)
	if err != nil || again.KeyID != kp.KeyID {
		t.Fatal(again, err)
	}
}
