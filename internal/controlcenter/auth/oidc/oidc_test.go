package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeOP is an in-process OpenID provider.
type fakeOP struct {
	srv      *httptest.Server
	key      *rsa.PrivateKey
	signKey  *rsa.PrivateKey // key actually used to sign (to test bad signatures)
	mu       sync.Mutex
	codes    map[string]codeInfo
	audience string
	nonce    string // override nonce in the ID token
	expired  bool
	groups   []string
}

type codeInfo struct{ challenge, nonce string }

func newFakeOP(t *testing.T) *fakeOP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	op := &fakeOP{key: key, signKey: key, codes: map[string]codeInfo{}, audience: "cc", groups: []string{"platform-admins", "devs"}}
	mux := http.NewServeMux()
	op.srv = httptest.NewServer(mux)
	t.Cleanup(op.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                op.srv.URL,
			"authorization_endpoint":                op.srv.URL + "/authorize",
			"token_endpoint":                        op.srv.URL + "/token",
			"jwks_uri":                              op.srv.URL + "/jwks",
			"userinfo_endpoint":                     op.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &op.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		op.mu.Lock()
		ci, ok := op.codes[r.Form.Get("code")]
		delete(op.codes, r.Form.Get("code"))
		op.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != ci.challenge {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		nonce := ci.nonce
		if op.nonce != "" {
			nonce = op.nonce
		}
		now := time.Now()
		exp := now.Add(time.Hour)
		if op.expired {
			exp = now.Add(-time.Hour)
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: op.signKey}, (&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
		claims := map[string]any{
			"iss": op.srv.URL, "sub": "user-123", "aud": op.audience, "iat": now.Unix(), "exp": exp.Unix(),
			"nonce": nonce, "email": "alice@example.com", "email_verified": true, "name": "Alice", "preferred_username": "alice",
			"groups": op.groups,
		}
		raw, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": raw})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"sub": "user-123", "email": "alice@example.com"})
	})
	return op
}

// authorize plays the browser and the provider's login page: it takes the
// URL Begin produced and returns the callback's code.
func (op *fakeOP) authorize(t *testing.T, authURL string) (state, code string) {
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		t.Fatalf("auth URL lacks PKCE or nonce: %s", authURL)
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("scope %q", q.Get("scope"))
	}
	code = "code-" + q.Get("state")[:8]
	op.mu.Lock()
	op.codes[code] = codeInfo{challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}
	op.mu.Unlock()
	return q.Get("state"), code
}

func newProvider(t *testing.T, op *fakeOP) *Provider {
	p, err := New(context.Background(), Config{Name: "kc", Issuer: op.srv.URL, ClientID: "cc", ClientSecret: "s", RedirectURL: "https://cc/auth/oidc/kc/callback"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOIDCSignIn(t *testing.T) {
	op := newFakeOP(t)
	p := newProvider(t, op)
	authURL, flow := p.Begin("/fleet")
	state, code := op.authorize(t, authURL)
	id, err := p.Finish(context.Background(), flow, state, code, "")
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if id.Subject != "user-123" || id.Email != "alice@example.com" || len(id.Groups) != 2 || id.Groups[0] != "platform-admins" {
		t.Fatalf("identity %+v", id)
	}
	// A code is single use.
	if _, err := p.Finish(context.Background(), flow, state, code, ""); err == nil {
		t.Fatal("code replay accepted")
	}
}

func TestOIDCNegative(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(op *fakeOP)
		mut   func(f *Flow, state, code *string)
	}{
		{"state mismatch", nil, func(f *Flow, state, code *string) { *state = "other" }},
		{"wrong PKCE verifier", nil, func(f *Flow, state, code *string) { f.Verifier = "x" + f.Verifier[1:] }},
		{"nonce mismatch", func(op *fakeOP) { op.nonce = "evil" }, nil},
		{"wrong audience", func(op *fakeOP) { op.audience = "someone-else" }, nil},
		{"expired token", func(op *fakeOP) { op.expired = true }, nil},
		{"bad signature", func(op *fakeOP) { k, _ := rsa.GenerateKey(rand.Reader, 2048); op.signKey = k }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := newFakeOP(t)
			if c.setup != nil {
				c.setup(op)
			}
			p := newProvider(t, op)
			authURL, flow := p.Begin("/")
			state, code := op.authorize(t, authURL)
			if c.mut != nil {
				c.mut(&flow, &state, &code)
			}
			if id, err := p.Finish(ctx, flow, state, code, ""); err == nil {
				t.Fatalf("accepted: %+v", id)
			}
		})
	}
	op := newFakeOP(t)
	p := newProvider(t, op)
	if _, err := p.Finish(ctx, Flow{State: "s"}, "s", "", "access_denied"); err == nil {
		t.Fatal("provider error accepted")
	}
}
