package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/local"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

type authSvc struct{ *Server }

func (s authSvc) ListProviders(ctx context.Context, _ *connect.Request[ccv1.ListProvidersRequest]) (*connect.Response[ccv1.ListProvidersResponse], error) {
	out := &ccv1.ListProvidersResponse{}
	p, err := s.K.Platform(ctx)
	if err != nil && !isNotFound(err) {
		return nil, kerr(err)
	}
	localOn := true
	if p != nil {
		out.AdminClaimed = p.Status.AdminClaimed
		localOn = !p.Spec.Auth.Local.Disabled
	}
	if localOn {
		if _, err := s.K.GetSecret(ctx, k8s.SecretLocalAdmin); err == nil {
			out.Providers = append(out.Providers, &ccv1.Provider{Name: "local", Kind: ccv1.ProviderKind_PROVIDER_KIND_LOCAL, DisplayName: "Local account"})
		}
	}
	if p != nil {
		for _, o := range p.Spec.Auth.OIDC {
			out.Providers = append(out.Providers, &ccv1.Provider{Name: o.Name, Kind: ccv1.ProviderKind_PROVIDER_KIND_OIDC,
				DisplayName: displayName(o.DisplayName, o.Name), LoginUrl: "/auth/oidc/" + o.Name + "/login"})
		}
		for _, sp := range p.Spec.Auth.SAML {
			out.Providers = append(out.Providers, &ccv1.Provider{Name: sp.Name, Kind: ccv1.ProviderKind_PROVIDER_KIND_SAML,
				DisplayName: displayName(sp.DisplayName, sp.Name), LoginUrl: "/auth/saml/" + sp.Name + "/login"})
		}
	}
	return connect.NewResponse(out), nil
}

func displayName(d, n string) string {
	if d != "" {
		return d
	}
	return n
}

func (s authSvc) LocalLogin(ctx context.Context, req *connect.Request[ccv1.LocalLoginRequest]) (*connect.Response[ccv1.LocalLoginResponse], error) {
	ip := clientIP(ctx, req.Header(), req.Peer().Addr)
	user := req.Msg.GetUsername()
	fail := func(result string, err error) error {
		s.Audit.Record(ctx, store.AuditEvent{Actor: user, Action: "auth.login", Object: "provider/local", SourceIP: ip, Result: result})
		return err
	}
	if ok, wait := s.Logins.Allow(ip); !ok {
		return nil, fail("rate limited", connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("too many attempts; try again in %s", wait.Round(1e9))))
	}
	if p, err := s.K.Platform(ctx); err == nil && p.Spec.Auth.Local.Disabled {
		return nil, fail("local accounts disabled", connect.NewError(connect.CodePermissionDenied, errors.New("local accounts are turned off")))
	}
	data, err := s.K.GetSecret(ctx, k8s.SecretLocalAdmin)
	bad := connect.NewError(connect.CodeUnauthenticated, errors.New("wrong username or password"))
	if err != nil {
		// Spend the same time as a real check, so the response doesn't tell.
		_ = local.Verify("$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g", req.Msg.GetPassword())
		return nil, fail("no local account", bad)
	}
	match := local.Verify(string(data["passwordHash"]), req.Msg.GetPassword()) == nil
	if !match || !strings.EqualFold(string(data["username"]), user) {
		return nil, fail("wrong username or password", bad)
	}
	s.Logins.Reset(ip)
	res := connect.NewResponse(&ccv1.LocalLoginResponse{})
	p, err := s.startSession(ctx, cookieWriter{res.Header()}, requestFor(req), session.Data{
		Provider: "local", Subject: string(data["username"]), Handle: string(data["username"]), Email: string(data["email"]),
		Role: auth.RoleAdmin.String(),
	})
	if err != nil {
		return nil, fail(err.Error(), err)
	}
	s.Audit.Record(ctx, store.AuditEvent{Actor: p.Name(), Action: "auth.login", Object: "provider/local", SourceIP: ip})
	res.Msg.User = userProto(p)
	return res, nil
}

func (s authSvc) Logout(ctx context.Context, req *connect.Request[ccv1.LogoutRequest]) (*connect.Response[ccv1.LogoutResponse], error) {
	res := connect.NewResponse(&ccv1.LogoutResponse{})
	s.Sessions.Destroy(ctx, cookieWriter{res.Header()}, requestFor(req))
	if p := auth.FromContext(ctx); p != nil {
		s.audit(ctx, req, "auth.logout", "", "", nil, nil, nil)
	}
	return res, nil
}

func (s authSvc) GetMe(ctx context.Context, req *connect.Request[ccv1.GetMeRequest]) (*connect.Response[ccv1.GetMeResponse], error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	out := &ccv1.GetMeResponse{User: userProto(p)}
	if d, err := s.Sessions.Load(ctx, nil, requestFor(req)); err == nil {
		out.ExpiresAt = ts(d.Expires)
	}
	return connect.NewResponse(out), nil
}
