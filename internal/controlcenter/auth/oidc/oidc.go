// Package oidc signs people in with an OpenID Connect provider (ADR 0005):
// authorization code flow with PKCE (S256), state and nonce checks, using
// go-oidc v3 and x/oauth2. Tokens are not kept; only the identity is.
package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config describes one provider.
type Config struct {
	Name         string
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string // openid is always added
	GroupsClaim  string   // default "groups"
	HTTPClient   *http.Client
}

// Identity is who the provider says signed in.
type Identity struct {
	Subject           string
	Email             string
	EmailVerified     bool
	Name              string
	PreferredUsername string
	Groups            []string
}

// Flow is what must survive the redirect to the provider and back. The
// caller keeps it in a sealed, short-lived cookie.
type Flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
}

// Provider is a configured, discovered OIDC provider.
type Provider struct {
	cfg      Config
	oauth    oauth2.Config
	verifier *gooidc.IDTokenVerifier
	provider *gooidc.Provider
}

func (c Config) ctx(ctx context.Context) context.Context {
	if c.HTTPClient != nil {
		return gooidc.ClientContext(ctx, c.HTTPClient)
	}
	return ctx
}

// New discovers the provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	p, err := gooidc.NewProvider(cfg.ctx(ctx), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", cfg.Name, err)
	}
	scopes := []string{gooidc.ScopeOpenID, "profile", "email"}
	for _, s := range cfg.Scopes {
		if s != gooidc.ScopeOpenID && s != "profile" && s != "email" {
			scopes = append(scopes, s)
		}
	}
	return &Provider{
		cfg:      cfg,
		provider: p,
		verifier: p.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     p.Endpoint(),
			Scopes:       scopes,
		},
	}, nil
}

// Name is the provider's name.
func (p *Provider) Name() string { return p.cfg.Name }

// Begin starts a sign-in: it returns the provider URL to redirect to and the
// flow to keep until the callback.
func (p *Provider) Begin(returnTo string) (string, Flow) {
	f := Flow{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Return: returnTo}
	u := p.oauth.AuthCodeURL(f.State, gooidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier))
	return u, f
}

// Finish completes a sign-in from the callback's query parameters.
func (p *Provider) Finish(ctx context.Context, f Flow, state, code, errParam string) (*Identity, error) {
	if errParam != "" {
		return nil, fmt.Errorf("provider returned error %q", errParam)
	}
	if f.State == "" || state != f.State {
		return nil, errors.New("state mismatch")
	}
	if code == "" {
		return nil, errors.New("missing code")
	}
	ctx = p.cfg.ctx(ctx)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, errors.New("no id_token in token response")
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("id token: %w", err)
	}
	if idt.Nonce != f.Nonce {
		return nil, errors.New("nonce mismatch")
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, err
	}
	id := &Identity{Subject: idt.Subject}
	fill(id, claims, p.cfg.GroupsClaim)
	// Groups or email may only be in userinfo.
	if (id.Groups == nil || id.Email == "") && p.provider.UserInfoEndpoint() != "" {
		if ui, err := p.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil && ui.Subject == idt.Subject {
			uc := map[string]any{}
			if ui.Claims(&uc) == nil {
				fill(id, uc, p.cfg.GroupsClaim)
			}
		}
	}
	return id, nil
}

func fill(id *Identity, c map[string]any, groupsClaim string) {
	if s, ok := c["email"].(string); ok && id.Email == "" {
		id.Email = s
		id.EmailVerified, _ = c["email_verified"].(bool)
	}
	if s, ok := c["name"].(string); ok && id.Name == "" {
		id.Name = s
	}
	if s, ok := c["preferred_username"].(string); ok && id.PreferredUsername == "" {
		id.PreferredUsername = s
	}
	if id.Groups == nil {
		switch g := c[groupsClaim].(type) {
		case []any:
			id.Groups = []string{}
			for _, v := range g {
				if s, ok := v.(string); ok {
					id.Groups = append(id.Groups, s)
				}
			}
		case string:
			id.Groups = []string{g}
		}
	}
}

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
