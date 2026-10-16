package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
)

type platformSvc struct{ *Server }

var providerName = regexp.MustCompile(`^[a-z0-9-]+$`)

// platformProto converts the Platform to the API's view (no secrets).
func (s *Server) platformProto(ctx context.Context, base string, p *v1alpha1.Platform) *ccv1.Platform {
	sp := p.Spec
	out := &ccv1.Platform{
		SignIn: &ccv1.SignInSettings{LocalEnabled: !sp.Auth.Local.Disabled, DefaultRole: roleProto(auth.ParseRole(string(sp.Auth.DefaultRole)))},
		Services: &ccv1.ServicesSettings{
			PostgresStorage: qstr(sp.Services.Postgres.Storage), ForgejoStorage: qstr(sp.Services.Forgejo.Storage),
			ForgejoOidcLogin: sp.Services.Forgejo.OIDCLogin, MessagingReplicas: sp.Services.Messaging.Replicas,
			MessagingStorage: qstr(sp.Services.Messaging.Storage), RegistryCacheStorage: qstr(sp.Services.RegistryCache.Storage),
			RegistryUpstreams: sp.Services.RegistryCache.Upstreams, NtfyEnabled: sp.Services.Ntfy.Enabled, Approved: sp.Wizard.ServicesApproved,
		},
		Hostname:   &ccv1.HostnameSettings{Hostname: sp.Hostname, TlsMode: string(sp.TLS.Mode)},
		WizardStep: sp.Wizard.Step,
		Status: &ccv1.PlatformStatus{AdminClaimed: p.Status.AdminClaimed, Url: p.Status.URL, CaFingerprint: p.Status.CAFingerprint,
			Conditions: conditions(p.Status.Conditions), Versions: p.Status.Versions},
	}
	if sp.TLS.IssuerRef != nil {
		out.Hostname.IssuerName, out.Hostname.IssuerKind = sp.TLS.IssuerRef.Name, sp.TLS.IssuerRef.Kind
	}
	for _, o := range sp.Auth.OIDC {
		out.SignIn.Oidc = append(out.SignIn.Oidc, &ccv1.OIDCProviderSettings{Name: o.Name, DisplayName: o.DisplayName, Issuer: o.Issuer,
			ClientId: o.ClientID, ClientSecretSet: o.ClientSecretRef.Name != "", GroupsClaim: o.GroupsClaim, Scopes: o.Scopes,
			RedirectUrl: base + "/auth/oidc/" + o.Name + "/callback"})
	}
	for _, sa := range sp.Auth.SAML {
		out.SignIn.Saml = append(out.SignIn.Saml, &ccv1.SAMLProviderSettings{Name: sa.Name, DisplayName: sa.DisplayName, MetadataUrl: sa.MetadataURL,
			GroupsAttribute: sa.GroupsAttribute, SpMetadataUrl: base + "/auth/saml/" + sa.Name + "/metadata", AcsUrl: base + "/auth/saml/" + sa.Name + "/acs"})
	}
	for _, m := range sp.Auth.RoleMappings {
		out.SignIn.RoleMappings = append(out.SignIn.RoleMappings, &ccv1.RoleMapping{Group: m.Group, Role: roleProto(auth.ParseRole(string(m.Role)))})
	}
	for _, pr := range sp.Services.Gateway.Providers {
		out.Services.Providers = append(out.Services.Providers, &ccv1.ModelProviderSettings{Name: pr.Name, Kind: string(pr.Kind),
			BaseUrl: pr.BaseURL, Models: pr.Models, ApiKeySet: pr.APIKeySecretRef != nil})
	}
	if pf := p.Status.Preflight; pf != nil {
		out.Status.Tier = string(pf.Tier)
		out.Status.RoomEstimate = pf.RoomEstimate
		out.Status.PreflightDone = pf.CompletedAt != nil
		for _, c := range pf.Checks {
			out.Status.Preflight = append(out.Status.Preflight, &ccv1.PreflightCheck{Name: c.Name, Result: string(c.Result), Message: c.Message, Fix: c.Fix})
		}
	}
	return out
}

func qstr(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	return q.String()
}

func qparse(s string) (*resource.Quantity, error) {
	if s == "" {
		return nil, nil
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return nil, errInvalid("%q is not a size like 20Gi", s)
	}
	return &q, nil
}

func baseFromHeaders(s *Server, h http.Header, host string) string {
	r := &http.Request{Header: h, Host: host}
	return s.publicURL(r)
}

func (s platformSvc) GetPlatform(ctx context.Context, req *connect.Request[ccv1.GetPlatformRequest]) (*connect.Response[ccv1.GetPlatformResponse], error) {
	if _, err := requireRole(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	p, err := s.K.Platform(ctx)
	if err != nil {
		return nil, kerr(err)
	}
	return connect.NewResponse(&ccv1.GetPlatformResponse{Platform: s.platformProto(ctx, baseFromHeaders(s.Server, req.Header(), req.Header().Get("Host")), p)}), nil
}

// update applies mutate to the Platform as an admin, audited with before and after.
func (s platformSvc) update(ctx context.Context, req connect.AnyRequest, action string, mutate func(p *v1alpha1.Platform) error) (*ccv1.Platform, error) {
	if _, err := requireRole(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	var before v1alpha1.PlatformSpec
	p, err := s.K.UpdatePlatform(ctx, func(p *v1alpha1.Platform) error {
		before = *p.Spec.DeepCopy()
		return mutate(p)
	})
	if p != nil {
		s.audit(ctx, req, action, "platform/default", "", before, p.Spec, err)
	} else {
		s.audit(ctx, req, action, "platform/default", "", nil, nil, err)
	}
	if err != nil {
		if ce, ok := err.(*connect.Error); ok {
			return nil, ce
		}
		return nil, kerr(err)
	}
	return s.platformProto(ctx, baseFromHeaders(s.Server, req.Header(), req.Header().Get("Host")), p), nil
}

func (s platformSvc) UpdateSignIn(ctx context.Context, req *connect.Request[ccv1.UpdateSignInRequest]) (*connect.Response[ccv1.UpdateSignInResponse], error) {
	in := req.Msg.GetSignIn()
	if in == nil {
		return nil, errInvalid("sign_in is required")
	}
	// Validate and write the referenced Secrets and ConfigMaps first.
	for _, o := range in.GetOidc() {
		if !providerName.MatchString(o.GetName()) || o.GetIssuer() == "" || o.GetClientId() == "" {
			return nil, errInvalid("OIDC provider %q needs a lower-case name, an issuer and a client ID", o.GetName())
		}
	}
	for _, sa := range in.GetSaml() {
		if !providerName.MatchString(sa.GetName()) || (sa.GetMetadataUrl() == "" && sa.GetMetadataXml() == "") {
			return nil, errInvalid("SAML provider %q needs a lower-case name and IdP metadata (URL or XML)", sa.GetName())
		}
	}
	if _, err := requireRole(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	for _, o := range in.GetOidc() {
		if o.GetClientSecret() != "" {
			if err := s.K.ApplySecret(ctx, "oidc-"+o.GetName(), map[string][]byte{"clientSecret": []byte(o.GetClientSecret())}); err != nil {
				return nil, kerr(err)
			}
		}
	}
	for _, sa := range in.GetSaml() {
		if sa.GetMetadataXml() != "" {
			if err := s.K.ApplyConfigMap(ctx, "saml-"+sa.GetName(), map[string]string{"metadata.xml": sa.GetMetadataXml()}); err != nil {
				return nil, kerr(err)
			}
		}
	}
	p, err := s.update(ctx, req, "platform.signIn", func(p *v1alpha1.Platform) error {
		a := &p.Spec.Auth
		a.Local.Disabled = !in.GetLocalEnabled()
		old := map[string]v1alpha1.OIDCProvider{}
		for _, o := range a.OIDC {
			old[o.Name] = o
		}
		a.OIDC = nil
		for _, o := range in.GetOidc() {
			np := v1alpha1.OIDCProvider{Name: o.GetName(), DisplayName: o.GetDisplayName(), Issuer: o.GetIssuer(), ClientID: o.GetClientId(),
				GroupsClaim: o.GetGroupsClaim(), Scopes: o.GetScopes(), ClientSecretRef: old[o.GetName()].ClientSecretRef}
			if o.GetClientSecret() != "" {
				np.ClientSecretRef = corev1.LocalObjectReference{Name: "oidc-" + o.GetName()}
			}
			if np.ClientSecretRef.Name == "" {
				return errInvalid("OIDC provider %q needs a client secret", o.GetName())
			}
			a.OIDC = append(a.OIDC, np)
		}
		oldSAML := map[string]v1alpha1.SAMLProvider{}
		for _, sa := range a.SAML {
			oldSAML[sa.Name] = sa
		}
		a.SAML = nil
		for _, sa := range in.GetSaml() {
			np := v1alpha1.SAMLProvider{Name: sa.GetName(), DisplayName: sa.GetDisplayName(), MetadataURL: sa.GetMetadataUrl(),
				GroupsAttribute: sa.GetGroupsAttribute(), MetadataConfigMapRef: oldSAML[sa.GetName()].MetadataConfigMapRef}
			if sa.GetMetadataXml() != "" {
				np.MetadataConfigMapRef = &corev1.LocalObjectReference{Name: "saml-" + sa.GetName()}
			}
			if np.MetadataURL != "" && sa.GetMetadataXml() == "" {
				np.MetadataConfigMapRef = nil
			}
			a.SAML = append(a.SAML, np)
		}
		a.RoleMappings = nil
		for _, m := range in.GetRoleMappings() {
			r := roleFromProto(m.GetRole())
			if m.GetGroup() == "" || r == "" {
				return errInvalid("role mappings need a group and a role")
			}
			a.RoleMappings = append(a.RoleMappings, v1alpha1.RoleMapping{Group: m.GetGroup(), Role: r})
		}
		a.DefaultRole = roleFromProto(in.GetDefaultRole())
		if a.Local.Disabled && len(a.OIDC) == 0 && len(a.SAML) == 0 {
			return errInvalid("turning local accounts off needs an identity provider")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.UpdateSignInResponse{Platform: p}), nil
}

func (s platformSvc) UpdateServices(ctx context.Context, req *connect.Request[ccv1.UpdateServicesRequest]) (*connect.Response[ccv1.UpdateServicesResponse], error) {
	in := req.Msg.GetServices()
	if in == nil {
		return nil, errInvalid("services is required")
	}
	if _, err := requireRole(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	sizes := map[string]*resource.Quantity{}
	for k, v := range map[string]string{"postgres": in.GetPostgresStorage(), "forgejo": in.GetForgejoStorage(),
		"messaging": in.GetMessagingStorage(), "registry": in.GetRegistryCacheStorage()} {
		q, err := qparse(v)
		if err != nil {
			return nil, err
		}
		sizes[k] = q
	}
	var secrets []string
	for _, pr := range in.GetProviders() {
		if !providerName.MatchString(pr.GetName()) {
			return nil, errInvalid("provider %q: lower case letters, digits and dashes", pr.GetName())
		}
		switch pr.GetKind() {
		case "anthropic", "openai", "openaiCompatible":
		default:
			return nil, errInvalid("provider %q: kind must be anthropic, openai or openaiCompatible", pr.GetName())
		}
		if pr.GetApiKey() != "" {
			// The control center writes only in the system namespace; the
			// operator copies the key to the platform namespace for the gateway.
			name := "gateway-provider-" + pr.GetName()
			if err := s.K.ApplySecret(ctx, name, map[string][]byte{"apiKey": []byte(pr.GetApiKey())}); err != nil {
				return nil, kerr(err)
			}
			secrets = append(secrets, name)
		}
	}
	p, err := s.update(ctx, req, "platform.services", func(p *v1alpha1.Platform) error {
		sv := &p.Spec.Services
		if sizes["postgres"] != nil {
			sv.Postgres.Storage = sizes["postgres"]
		}
		if sizes["forgejo"] != nil {
			sv.Forgejo.Storage = sizes["forgejo"]
		}
		if sizes["messaging"] != nil {
			sv.Messaging.Storage = sizes["messaging"]
		}
		if sizes["registry"] != nil {
			sv.RegistryCache.Storage = sizes["registry"]
		}
		sv.Forgejo.OIDCLogin = in.GetForgejoOidcLogin()
		sv.Messaging.Replicas = in.GetMessagingReplicas()
		sv.RegistryCache.Upstreams = in.GetRegistryUpstreams()
		sv.Ntfy.Enabled = in.GetNtfyEnabled()
		old := map[string]*corev1.LocalObjectReference{}
		for _, pr := range sv.Gateway.Providers {
			old[pr.Name] = pr.APIKeySecretRef
		}
		sv.Gateway.Providers = nil
		for _, pr := range in.GetProviders() {
			mp := v1alpha1.ModelProvider{Name: pr.GetName(), Kind: v1alpha1.ProviderKind(pr.GetKind()), BaseURL: pr.GetBaseUrl(),
				Models: pr.GetModels(), APIKeySecretRef: old[pr.GetName()]}
			if pr.GetApiKey() != "" {
				mp.APIKeySecretRef = &corev1.LocalObjectReference{Name: "gateway-provider-" + pr.GetName()}
			}
			sv.Gateway.Providers = append(sv.Gateway.Providers, mp)
		}
		if in.GetApproved() {
			p.Spec.Wizard.ServicesApproved = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.UpdateServicesResponse{Platform: p, ProviderSecrets: secrets}), nil
}

var hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`)

func (s platformSvc) UpdateHostname(ctx context.Context, req *connect.Request[ccv1.UpdateHostnameRequest]) (*connect.Response[ccv1.UpdateHostnameResponse], error) {
	in := req.Msg.GetHostname()
	if in == nil {
		return nil, errInvalid("hostname is required")
	}
	if in.GetHostname() != "" && !hostnameRE.MatchString(in.GetHostname()) {
		return nil, errInvalid("%q is not a hostname", in.GetHostname())
	}
	mode := v1alpha1.TLSMode(in.GetTlsMode())
	if mode != "" && mode != v1alpha1.TLSSelfSigned && mode != v1alpha1.TLSCertManager {
		return nil, errInvalid("tls_mode must be selfSigned or certManager")
	}
	p, err := s.update(ctx, req, "platform.hostname", func(p *v1alpha1.Platform) error {
		p.Spec.Hostname = in.GetHostname()
		if mode != "" {
			p.Spec.TLS.Mode = mode
		}
		p.Spec.TLS.IssuerRef = nil
		if mode == v1alpha1.TLSCertManager {
			if in.GetIssuerName() == "" {
				return errInvalid("cert-manager needs an issuer")
			}
			p.Spec.TLS.IssuerRef = &v1alpha1.IssuerRef{Name: in.GetIssuerName(), Kind: in.GetIssuerKind()}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.UpdateHostnameResponse{Platform: p}), nil
}

func (s platformSvc) SetWizardStep(ctx context.Context, req *connect.Request[ccv1.SetWizardStepRequest]) (*connect.Response[ccv1.SetWizardStepResponse], error) {
	if len(req.Msg.GetStep()) > 64 {
		return nil, errInvalid("step name too long")
	}
	if _, err := s.update(ctx, req, "platform.wizardStep", func(p *v1alpha1.Platform) error {
		p.Spec.Wizard.Step = req.Msg.GetStep()
		return nil
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.SetWizardStepResponse{}), nil
}

// WatchPlatform polls the Platform and sends it whenever it changes.
func (s platformSvc) WatchPlatform(ctx context.Context, req *connect.Request[ccv1.WatchPlatformRequest], stream *connect.ServerStream[ccv1.WatchPlatformResponse]) error {
	if _, err := requireRole(ctx, auth.RoleViewer); err != nil {
		return err
	}
	base := baseFromHeaders(s.Server, req.Header(), req.Header().Get("Host"))
	last := ""
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		p, err := s.K.Platform(ctx)
		if err != nil && !isNotFound(err) {
			return kerr(err)
		}
		if p != nil && p.ResourceVersion != last {
			last = p.ResourceVersion
			if err := stream.Send(&ccv1.WatchPlatformResponse{Platform: s.platformProto(ctx, base, p)}); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

var _ = fmt.Sprint
