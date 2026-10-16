// Package saml signs people in with a SAML 2.0 identity provider (ADR 0005),
// as a service provider built on gosaml2: SP-initiated, signed AuthnRequests
// (HTTP-Redirect binding), responses at the ACS by HTTP-POST.
//
// On top of gosaml2's checks (XML signature, issuer, destination, recipient,
// subject confirmation expiry, status), this package requires:
//   - exactly one assertion, covered by a valid signature (a signed Response
//     or a signed Assertion); gosaml2 rejects documents with both signed and
//     unsigned assertions
//   - InResponseTo naming an AuthnRequest we issued and have not seen
//     answered yet (single use, so a response can't be replayed)
//   - an AudienceRestriction naming our entity ID
//   - NotBefore/NotOnOrAfter around now (gosaml2 only warns about these)
package saml

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	saml2 "github.com/russellhaering/gosaml2"
	"github.com/russellhaering/gosaml2/types"
	dsig "github.com/russellhaering/goxmldsig"
)

// RequestTTL is how long an AuthnRequest may be answered.
const RequestTTL = 10 * time.Minute

// RequestStore remembers issued AuthnRequest IDs.
type RequestStore interface {
	Save(ctx context.Context, id string, expires time.Time) error
	// Consume removes id and reports whether it was outstanding.
	Consume(ctx context.Context, id string) (bool, error)
}

// Config describes one provider.
type Config struct {
	Name            string
	EntityID        string // our SP entity ID (our metadata URL)
	ACSURL          string
	IdPMetadata     []byte
	GroupsAttribute string // default "groups"
	SPKey           crypto.Signer
	SPCert          []byte // DER
	Clock           *dsig.Clock
}

// Identity is who the IdP says signed in.
type Identity struct {
	NameID      string
	Email       string
	DisplayName string
	Username    string
	Groups      []string
}

// Provider is a configured SAML IdP.
type Provider struct {
	name     string
	groups   string
	sp       *saml2.SAMLServiceProvider
	requests RequestStore
	clock    *dsig.Clock
}

// New parses the IdP metadata and builds the service provider.
func New(cfg Config, requests RequestStore) (*Provider, error) {
	var md types.EntityDescriptor
	if err := xml.Unmarshal(cfg.IdPMetadata, &md); err != nil {
		return nil, fmt.Errorf("idp metadata: %w", err)
	}
	if md.IDPSSODescriptor == nil {
		return nil, errors.New("idp metadata has no IDPSSODescriptor")
	}
	ssoURL := ""
	for _, s := range md.IDPSSODescriptor.SingleSignOnServices {
		if s.Binding == saml2.BindingHttpRedirect {
			ssoURL = s.Location
		}
	}
	if ssoURL == "" {
		return nil, errors.New("idp metadata has no HTTP-Redirect SingleSignOnService")
	}
	store := &dsig.MemoryX509CertificateStore{}
	for _, kd := range md.IDPSSODescriptor.KeyDescriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, xc := range kd.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(xc.Data), ""))
			if err != nil {
				return nil, fmt.Errorf("idp certificate: %w", err)
			}
			c, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, fmt.Errorf("idp certificate: %w", err)
			}
			store.Roots = append(store.Roots, c)
		}
	}
	if len(store.Roots) == 0 {
		return nil, errors.New("idp metadata has no signing certificate")
	}
	if cfg.GroupsAttribute == "" {
		cfg.GroupsAttribute = "groups"
	}
	clock := cfg.Clock
	if clock == nil {
		clock = dsig.NewRealClock()
	}
	sp := &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL:      ssoURL,
		IdentityProviderSSOBinding:  saml2.BindingHttpRedirect,
		IdentityProviderIssuer:      md.EntityID,
		AssertionConsumerServiceURL: cfg.ACSURL,
		ServiceProviderIssuer:       cfg.EntityID,
		AudienceURI:                 cfg.EntityID,
		SignAuthnRequests:           true,
		SignAuthnRequestsAlgorithm:  dsig.RSASHA256SignatureMethod,
		IDPCertificateStore:         store,
		NameIdFormat:                "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent",
		AllowMissingAttributes:      true,
		Clock:                       clock,
		MaximumDecompressedBodySize: 1 << 20,
	}
	if err := sp.SetSPKeyStore(&saml2.KeyStore{Signer: cfg.SPKey, Cert: cfg.SPCert}); err != nil {
		return nil, err
	}
	return &Provider{name: cfg.Name, groups: cfg.GroupsAttribute, sp: sp, requests: requests, clock: clock}, nil
}

// Name is the provider's name.
func (p *Provider) Name() string { return p.name }

// Begin builds a signed AuthnRequest and returns the IdP URL to redirect to.
func (p *Provider) Begin(ctx context.Context, relayState string) (string, error) {
	doc, err := p.sp.BuildAuthRequestDocumentNoSig()
	if err != nil {
		return "", err
	}
	id := doc.Root().SelectAttrValue("ID", "")
	if id == "" {
		return "", errors.New("authn request has no ID")
	}
	if err := p.requests.Save(ctx, id, p.clock.Now().Add(RequestTTL)); err != nil {
		return "", err
	}
	return p.sp.BuildAuthURLRedirect(relayState, doc)
}

// Finish validates the base64 SAMLResponse posted to the ACS.
func (p *Provider) Finish(ctx context.Context, encoded string) (*Identity, error) {
	resp, err := p.sp.ValidateEncodedResponse(encoded)
	if err != nil {
		return nil, fmt.Errorf("saml response: %w", err)
	}
	if len(resp.Assertions) != 1 {
		return nil, fmt.Errorf("saml response has %d signed assertions, want exactly 1", len(resp.Assertions))
	}
	a := resp.Assertions[0]
	if !resp.SignatureValidated && !a.SignatureValidated {
		return nil, errors.New("saml assertion is not signed")
	}
	// InResponseTo: on the Response and on the subject confirmation, both
	// naming a request we issued. Consuming it makes the response single use.
	irt := resp.InResponseTo
	if sc := a.Subject; sc != nil && sc.SubjectConfirmation != nil && sc.SubjectConfirmation.SubjectConfirmationData != nil {
		if d := sc.SubjectConfirmation.SubjectConfirmationData.InResponseTo; d != "" {
			if irt != "" && d != irt {
				return nil, errors.New("saml InResponseTo differs between response and assertion")
			}
			irt = d
		}
	}
	if irt == "" {
		return nil, errors.New("saml response is unsolicited (no InResponseTo)")
	}
	ok, err := p.requests.Consume(ctx, irt)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("saml InResponseTo does not match an outstanding request")
	}
	if a.Conditions == nil || len(a.Conditions.AudienceRestrictions) == 0 {
		return nil, errors.New("saml assertion has no audience restriction")
	}
	warn, err := p.sp.VerifyAssertionConditions(&a)
	if err != nil {
		return nil, fmt.Errorf("saml conditions: %w", err)
	}
	if warn.NotInAudience {
		return nil, errors.New("saml assertion is not for this service provider (audience)")
	}
	if warn.InvalidTime {
		return nil, errors.New("saml assertion is outside its validity window")
	}
	if a.Subject == nil || a.Subject.NameID == nil || a.Subject.NameID.Value == "" {
		return nil, errors.New("saml assertion has no NameID")
	}
	id := &Identity{NameID: a.Subject.NameID.Value}
	if a.AttributeStatement != nil {
		for _, at := range a.AttributeStatement.Attributes {
			var vals []string
			for _, v := range at.Values {
				vals = append(vals, v.Value)
			}
			name := at.Name
			switch {
			case name == p.groups || at.FriendlyName == p.groups:
				id.Groups = append(id.Groups, vals...)
			case len(vals) == 0:
			case name == "email" || name == "mail" || name == "urn:oid:0.9.2342.19200300.100.1.3" ||
				name == "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress":
				id.Email = vals[0]
			case name == "displayName" || name == "name" || name == "urn:oid:2.16.840.1.113730.3.1.241":
				id.DisplayName = vals[0]
			case name == "username" || name == "uid" || name == "urn:oid:0.9.2342.19200300.100.1.1":
				id.Username = vals[0]
			}
		}
	}
	return id, nil
}

// Metadata returns our SP metadata XML.
func (p *Provider) Metadata() ([]byte, error) {
	md, err := p.sp.Metadata()
	if err != nil {
		return nil, err
	}
	return xml.MarshalIndent(md, "", "  ")
}
