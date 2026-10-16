package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/oidc"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/saml"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// idpCache keeps configured providers until their configuration changes.
type idpCache struct {
	mu   sync.Mutex
	oidc map[string]cachedOIDC
	saml map[string]cachedSAML
}

type cachedOIDC struct {
	key string
	p   *oidc.Provider
	at  time.Time
}

type cachedSAML struct {
	key string
	p   *saml.Provider
	at  time.Time
}

const flowCookie = "swarm_oidc_flow"

// safeReturn keeps post-sign-in redirects on this site.
func safeReturn(r string) string {
	if r == "" || !strings.HasPrefix(r, "/") || strings.HasPrefix(r, "//") || strings.HasPrefix(r, "/\\") {
		return "/"
	}
	return r
}

func (s *Server) oidcProvider(ctx context.Context, r *http.Request, name string) (*oidc.Provider, *v1alpha1.Platform, error) {
	pl, err := s.K.Platform(ctx)
	if err != nil {
		return nil, nil, err
	}
	var cfg *v1alpha1.OIDCProvider
	for i := range pl.Spec.Auth.OIDC {
		if pl.Spec.Auth.OIDC[i].Name == name {
			cfg = &pl.Spec.Auth.OIDC[i]
		}
	}
	if cfg == nil {
		return nil, nil, errors.New("no such OIDC provider")
	}
	secret := ""
	if cfg.ClientSecretRef.Name != "" {
		d, err := s.K.GetSecret(ctx, cfg.ClientSecretRef.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("client secret: %w", err)
		}
		secret = string(d["clientSecret"])
	}
	redirect := s.publicURL(r) + "/auth/oidc/" + name + "/callback"
	b, _ := json.Marshal(cfg)
	key := string(b) + "|" + secret + "|" + redirect
	s.idp.mu.Lock()
	defer s.idp.mu.Unlock()
	if s.idp.oidc == nil {
		s.idp.oidc = map[string]cachedOIDC{}
	}
	if c, ok := s.idp.oidc[name]; ok && c.key == key && time.Since(c.at) < time.Hour {
		return c.p, pl, nil
	}
	p, err := oidc.New(ctx, oidc.Config{Name: name, Issuer: cfg.Issuer, ClientID: cfg.ClientID, ClientSecret: secret,
		RedirectURL: redirect, Scopes: cfg.Scopes, GroupsClaim: cfg.GroupsClaim})
	if err != nil {
		return nil, nil, err
	}
	s.idp.oidc[name] = cachedOIDC{key: key, p: p, at: time.Now()}
	return p, pl, nil
}

// handleOIDC serves /auth/oidc/<name>/login and /callback.
func (s *Server) handleOIDC(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/auth/oidc/"), "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	name, step := parts[0], parts[1]
	ctx := r.Context()
	p, pl, err := s.oidcProvider(ctx, r, name)
	if err != nil {
		s.signInError(w, r, "oidc:"+name, err)
		return
	}
	switch step {
	case "login":
		u, flow := p.Begin(safeReturn(r.URL.Query().Get("return")))
		sealed, err := s.Sessions.Seal("oidc:"+name, flow, 10*time.Minute)
		if err != nil {
			s.signInError(w, r, "oidc:"+name, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: sealed, Path: "/auth/oidc/" + name + "/", MaxAge: 600,
			HttpOnly: true, Secure: s.Sessions.Secure, SameSite: http.SameSiteLaxMode})
		http.Redirect(w, r, u, http.StatusFound)
	case "callback":
		var flow oidc.Flow
		c, err := r.Cookie(flowCookie)
		if err != nil || s.Sessions.Unseal("oidc:"+name, c.Value, &flow) != nil {
			s.signInError(w, r, "oidc:"+name, errors.New("the sign-in took too long or started elsewhere; try again"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: "", Path: "/auth/oidc/" + name + "/", MaxAge: -1})
		q := r.URL.Query()
		id, err := p.Finish(ctx, flow, q.Get("state"), q.Get("code"), q.Get("error"))
		if err != nil {
			s.signInError(w, r, "oidc:"+name, err)
			return
		}
		handle := id.PreferredUsername
		if handle == "" {
			handle = id.Email
		}
		if handle == "" {
			handle = id.Subject
		}
		s.finishSignIn(w, r, pl, "oidc:"+name, id.Subject, handle, id.Email, id.Name, id.Groups, flow.Return)
	default:
		http.NotFound(w, r)
	}
}

// finishSignIn maps groups to a role, starts the session and redirects.
func (s *Server) finishSignIn(w http.ResponseWriter, r *http.Request, pl *v1alpha1.Platform, provider, subject, handle, email, name string, groups []string, ret string) {
	role := auth.MapRole(groups, pl.Spec.Auth.RoleMappings, pl.Spec.Auth.DefaultRole)
	ip := session.ClientIP(r)
	if role == auth.RoleNone {
		s.Audit.Record(r.Context(), store.AuditEvent{Actor: displayName(email, subject), Action: "auth.login", Object: "provider/" + provider,
			SourceIP: ip, Result: "no role for groups"})
		s.signInError(w, r, provider, errors.New("your account isn't mapped to a role here; ask an admin"))
		return
	}
	p, err := s.startSession(r.Context(), w, r, session.Data{Provider: provider, Subject: subject, Handle: store.HandleFrom(handle),
		Email: email, DisplayName: name, Groups: groups, Role: role.String()})
	if err != nil {
		s.signInError(w, r, provider, err)
		return
	}
	// The first admin may come from the identity provider's groups (§8.1):
	// signing in as one claims the installation if nobody has.
	if role == auth.RoleAdmin {
		if !pl.Status.AdminClaimed {
			if err := s.K.SetAdminClaimed(r.Context()); err == nil {
				s.Audit.Record(r.Context(), store.AuditEvent{Actor: p.Name(), Action: "setup.claim", Object: "provider/" + provider, SourceIP: ip})
			}
		}
	}
	s.Audit.Record(r.Context(), store.AuditEvent{Actor: p.Name(), Action: "auth.login", Object: "provider/" + provider, SourceIP: ip,
		Detail: jsonString(map[string]any{"role": p.Role.String(), "groups": groups})})
	http.Redirect(w, r, safeReturn(ret), http.StatusSeeOther)
}

func (s *Server) signInError(w http.ResponseWriter, r *http.Request, provider string, err error) {
	s.Log.Warn("sign-in failed", "provider", provider, "err", err)
	s.Audit.Record(r.Context(), store.AuditEvent{Actor: "anonymous", Action: "auth.login", Object: "provider/" + provider,
		SourceIP: session.ClientIP(r), Result: err.Error()})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Sign-in failed</title><p>Sign-in failed: %s</p><p><a href="/">Back</a></p>`,
		html.EscapeString(err.Error()))
}

// ---------------------------------------------------------------- SAML

// memSAMLRequests remembers AuthnRequest IDs before the database exists.
type memSAMLRequests struct {
	mu  sync.Mutex
	ids map[string]time.Time
}

type samlRequests struct {
	s        *Server
	provider string
}

func (q samlRequests) Save(ctx context.Context, id string, exp time.Time) error {
	if db := q.s.DB(); db != nil {
		return db.SaveSAMLRequest(ctx, id, q.provider, exp)
	}
	m := &q.s.saml
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ids == nil {
		m.ids = map[string]time.Time{}
	}
	for k, t := range m.ids {
		if time.Now().After(t) {
			delete(m.ids, k)
		}
	}
	m.ids[q.provider+"/"+id] = exp
	return nil
}

func (q samlRequests) Consume(ctx context.Context, id string) (bool, error) {
	m := &q.s.saml
	m.mu.Lock()
	exp, ok := m.ids[q.provider+"/"+id]
	delete(m.ids, q.provider+"/"+id)
	m.mu.Unlock()
	if ok {
		return time.Now().Before(exp), nil
	}
	if db := q.s.DB(); db != nil {
		return db.ConsumeSAMLRequest(ctx, id, q.provider)
	}
	return false, nil
}

// spKey returns the SAML SP key pair, creating it on first use.
func (s *Server) spKey(ctx context.Context) (*rsa.PrivateKey, []byte, error) {
	d, err := s.K.GetSecret(ctx, k8s.SecretSAML)
	if err != nil {
		if !isNotFound(err) {
			return nil, nil, err
		}
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, nil, err
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "swarmageddon control center SAML"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
		if err != nil {
			return nil, nil, err
		}
		d, err = s.K.CreateSecretIfMissing(ctx, k8s.SecretSAML, map[string][]byte{
			"tls.key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}),
			"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		})
		if err != nil {
			return nil, nil, err
		}
	}
	kb, _ := pem.Decode(d["tls.key"])
	cb, _ := pem.Decode(d["tls.crt"])
	if kb == nil || cb == nil {
		return nil, nil, errors.New("SAML key Secret is malformed")
	}
	k, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return k, cb.Bytes, nil
}

func (s *Server) samlMetadata(ctx context.Context, cfg *v1alpha1.SAMLProvider) ([]byte, error) {
	if cfg.MetadataConfigMapRef != nil {
		d, err := s.K.GetConfigMap(ctx, cfg.MetadataConfigMapRef.Name)
		if err != nil {
			return nil, err
		}
		return []byte(d["metadata.xml"]), nil
	}
	if cfg.MetadataURL == "" {
		return nil, errors.New("no IdP metadata configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.MetadataURL, nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("IdP metadata: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (s *Server) samlProvider(ctx context.Context, r *http.Request, name string) (*saml.Provider, *v1alpha1.Platform, error) {
	pl, err := s.K.Platform(ctx)
	if err != nil {
		return nil, nil, err
	}
	var cfg *v1alpha1.SAMLProvider
	for i := range pl.Spec.Auth.SAML {
		if pl.Spec.Auth.SAML[i].Name == name {
			cfg = &pl.Spec.Auth.SAML[i]
		}
	}
	if cfg == nil {
		return nil, nil, errors.New("no such SAML provider")
	}
	base := s.publicURL(r)
	b, _ := json.Marshal(cfg)
	key := string(b) + "|" + base
	s.idp.mu.Lock()
	if c, ok := s.idp.saml[name]; ok && c.key == key && time.Since(c.at) < time.Hour {
		s.idp.mu.Unlock()
		return c.p, pl, nil
	}
	s.idp.mu.Unlock()
	md, err := s.samlMetadata(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	k, cert, err := s.spKey(ctx)
	if err != nil {
		return nil, nil, err
	}
	p, err := saml.New(saml.Config{Name: name, EntityID: base + "/auth/saml/" + name + "/metadata", ACSURL: base + "/auth/saml/" + name + "/acs",
		IdPMetadata: md, GroupsAttribute: cfg.GroupsAttribute, SPKey: k, SPCert: cert}, samlRequests{s: s, provider: name})
	if err != nil {
		return nil, nil, err
	}
	s.idp.mu.Lock()
	if s.idp.saml == nil {
		s.idp.saml = map[string]cachedSAML{}
	}
	s.idp.saml[name] = cachedSAML{key: key, p: p, at: time.Now()}
	s.idp.mu.Unlock()
	return p, pl, nil
}

// handleSAML serves /auth/saml/<name>/login, /acs and /metadata.
func (s *Server) handleSAML(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/auth/saml/"), "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	name, step := parts[0], parts[1]
	ctx := r.Context()
	p, pl, err := s.samlProvider(ctx, r, name)
	if err != nil {
		s.signInError(w, r, "saml:"+name, err)
		return
	}
	switch step {
	case "metadata":
		md, err := p.Metadata()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write(md)
	case "login":
		u, err := p.Begin(ctx, safeReturn(r.URL.Query().Get("return")))
		if err != nil {
			s.signInError(w, r, "saml:"+name, err)
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
	case "acs":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		if err := r.ParseForm(); err != nil {
			s.signInError(w, r, "saml:"+name, err)
			return
		}
		id, err := p.Finish(ctx, r.PostForm.Get("SAMLResponse"))
		if err != nil {
			s.signInError(w, r, "saml:"+name, err)
			return
		}
		handle := id.Username
		if handle == "" {
			handle = id.Email
		}
		if handle == "" {
			handle = id.NameID
		}
		s.finishSignIn(w, r, pl, "saml:"+name, id.NameID, handle, id.Email, id.DisplayName, id.Groups, r.PostForm.Get("RelayState"))
	default:
		http.NotFound(w, r)
	}
}
