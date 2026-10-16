package saml

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	spEntity = "https://cc.example/auth/saml/kc/metadata"
	acsURL   = "https://cc.example/auth/saml/kc/acs"
	idpID    = "https://idp.example/realms/test"
)

type memRequests struct {
	mu  sync.Mutex
	ids map[string]time.Time
}

func (m *memRequests) Save(_ context.Context, id string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ids[id] = exp
	return nil
}

func (m *memRequests) Consume(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.ids[id]
	delete(m.ids, id)
	return ok && time.Now().Before(exp), nil
}

func keyPair(t *testing.T, cn string) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return k, der
}

type idp struct {
	key  *rsa.PrivateKey
	cert []byte
}

func (i idp) metadata() []byte {
	return []byte(fmt.Sprintf(`<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
<md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
<md:KeyDescriptor use="signing"><ds:KeyInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>
<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/>
<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="https://idp.example/sso-post"/>
</md:IDPSSODescriptor></md:EntityDescriptor>`, idpID, base64.StdEncoding.EncodeToString(i.cert)))
}

func (i idp) signer() *dsig.SigningContext {
	ctx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore(tls.Certificate{Certificate: [][]byte{i.cert}, PrivateKey: i.key}))
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	_ = ctx.SetSignatureMethod(dsig.RSASHA256SignatureMethod)
	return ctx
}

// opts shape one test response.
type opts struct {
	irt          string
	audience     string
	nameID       string
	notBefore    time.Time
	notOnOrAfter time.Time
	signAssert   bool
	signResp     bool
	noAudience   bool
}

const ts = "2006-01-02T15:04:05Z"

func assertion(o opts) *etree.Element {
	now := time.Now().UTC()
	a := etree.NewElement("saml:Assertion")
	a.CreateAttr("xmlns:saml", "urn:oasis:names:tc:SAML:2.0:assertion")
	a.CreateAttr("ID", "_a"+fmt.Sprint(now.UnixNano()))
	a.CreateAttr("Version", "2.0")
	a.CreateAttr("IssueInstant", now.Format(ts))
	a.CreateElement("saml:Issuer").SetText(idpID)
	subj := a.CreateElement("saml:Subject")
	nid := subj.CreateElement("saml:NameID")
	nid.CreateAttr("Format", "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent")
	nid.SetText(o.nameID)
	sc := subj.CreateElement("saml:SubjectConfirmation")
	sc.CreateAttr("Method", "urn:oasis:names:tc:SAML:2.0:cm:bearer")
	scd := sc.CreateElement("saml:SubjectConfirmationData")
	scd.CreateAttr("InResponseTo", o.irt)
	scd.CreateAttr("Recipient", acsURL)
	scd.CreateAttr("NotOnOrAfter", now.Add(5*time.Minute).Format(ts))
	cond := a.CreateElement("saml:Conditions")
	cond.CreateAttr("NotBefore", o.notBefore.UTC().Format(ts))
	cond.CreateAttr("NotOnOrAfter", o.notOnOrAfter.UTC().Format(ts))
	if !o.noAudience {
		cond.CreateElement("saml:AudienceRestriction").CreateElement("saml:Audience").SetText(o.audience)
	}
	as := a.CreateElement("saml:AuthnStatement")
	as.CreateAttr("AuthnInstant", now.Format(ts))
	st := a.CreateElement("saml:AttributeStatement")
	attr := st.CreateElement("saml:Attribute")
	attr.CreateAttr("Name", "groups")
	attr.CreateElement("saml:AttributeValue").SetText("platform-admins")
	attr.CreateElement("saml:AttributeValue").SetText("devs")
	em := st.CreateElement("saml:Attribute")
	em.CreateAttr("Name", "email")
	em.CreateElement("saml:AttributeValue").SetText("bob@example.com")
	return a
}

func response(t *testing.T, i idp, o opts, a *etree.Element) *etree.Element {
	t.Helper()
	now := time.Now().UTC()
	if o.signAssert {
		signed, err := i.signer().SignEnveloped(a)
		if err != nil {
			t.Fatal(err)
		}
		a = signed
	}
	r := etree.NewElement("samlp:Response")
	r.CreateAttr("xmlns:samlp", "urn:oasis:names:tc:SAML:2.0:protocol")
	r.CreateAttr("xmlns:saml", "urn:oasis:names:tc:SAML:2.0:assertion")
	r.CreateAttr("ID", "_r"+fmt.Sprint(now.UnixNano()))
	r.CreateAttr("Version", "2.0")
	r.CreateAttr("IssueInstant", now.Format(ts))
	r.CreateAttr("Destination", acsURL)
	if o.irt != "" {
		r.CreateAttr("InResponseTo", o.irt)
	}
	r.CreateElement("saml:Issuer").SetText(idpID)
	r.CreateElement("samlp:Status").CreateElement("samlp:StatusCode").CreateAttr("Value", "urn:oasis:names:tc:SAML:2.0:status:Success")
	r.AddChild(a)
	if o.signResp {
		signed, err := i.signer().SignEnveloped(r)
		if err != nil {
			t.Fatal(err)
		}
		r = signed
	}
	return r
}

func encode(t *testing.T, el *etree.Element) string {
	t.Helper()
	doc := etree.NewDocument()
	doc.SetRoot(el.Copy())
	s, err := doc.WriteToString()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func setup(t *testing.T) (*Provider, idp, *memRequests) {
	ik, ic := keyPair(t, "idp")
	sk, sc := keyPair(t, "sp")
	i := idp{key: ik, cert: ic}
	reqs := &memRequests{ids: map[string]time.Time{}}
	p, err := New(Config{Name: "kc", EntityID: spEntity, ACSURL: acsURL, IdPMetadata: i.metadata(), SPKey: sk, SPCert: sc}, reqs)
	if err != nil {
		t.Fatal(err)
	}
	return p, i, reqs
}

// begin runs Begin and returns the AuthnRequest ID, checking it is signed.
func begin(t *testing.T, p *Provider) string {
	t.Helper()
	u, err := p.Begin(context.Background(), "/fleet")
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	q := pu.Query()
	if q.Get("Signature") == "" || !strings.Contains(q.Get("SigAlg"), "rsa-sha256") || q.Get("RelayState") != "/fleet" {
		t.Fatalf("AuthnRequest not signed: %s", u)
	}
	raw, _ := base64.StdEncoding.DecodeString(q.Get("SAMLRequest"))
	xmlb, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(xmlb); err != nil {
		t.Fatal(err)
	}
	return doc.Root().SelectAttrValue("ID", "")
}

func good(irt string) opts {
	return opts{irt: irt, audience: spEntity, nameID: "bob", notBefore: time.Now().Add(-time.Minute),
		notOnOrAfter: time.Now().Add(5 * time.Minute), signAssert: true}
}

func TestSAMLSignIn(t *testing.T) {
	for _, mode := range []string{"signed assertion", "signed response"} {
		t.Run(mode, func(t *testing.T) {
			p, i, _ := setup(t)
			id := begin(t, p)
			o := good(id)
			if mode == "signed response" {
				o.signAssert, o.signResp = false, true
			}
			got, err := p.Finish(context.Background(), encode(t, response(t, i, o, assertion(o))))
			if err != nil {
				t.Fatalf("finish: %v", err)
			}
			if got.NameID != "bob" || got.Email != "bob@example.com" || len(got.Groups) != 2 {
				t.Fatalf("identity %+v", got)
			}
		})
	}
	p, _, _ := setup(t)
	md, err := p.Metadata()
	if err != nil || !bytes.Contains(md, []byte(acsURL)) || !bytes.Contains(md, []byte(`AuthnRequestsSigned="true"`)) {
		t.Fatalf("metadata: %s %v", md, err)
	}
}

func TestSAMLNegative(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		build func(t *testing.T, i idp, id string) string
	}{
		{"unsigned", func(t *testing.T, i idp, id string) string {
			o := good(id)
			o.signAssert = false
			return encode(t, response(t, i, o, assertion(o)))
		}},
		{"wrong audience", func(t *testing.T, i idp, id string) string {
			o := good(id)
			o.audience = "https://evil.example/sp"
			return encode(t, response(t, i, o, assertion(o)))
		}},
		{"no audience", func(t *testing.T, i idp, id string) string {
			o := good(id)
			o.noAudience = true
			return encode(t, response(t, i, o, assertion(o)))
		}},
		{"unknown InResponseTo", func(t *testing.T, i idp, id string) string {
			o := good("_never-issued")
			return encode(t, response(t, i, o, assertion(o)))
		}},
		{"unsolicited", func(t *testing.T, i idp, id string) string {
			o := good("")
			return encode(t, response(t, i, o, assertion(o)))
		}},
		{"expired", func(t *testing.T, i idp, id string) string {
			o := good(id)
			o.notBefore, o.notOnOrAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
			return encode(t, response(t, i, o, assertion(o)))
		}},
		{"not yet valid", func(t *testing.T, i idp, id string) string {
			o := good(id)
			o.notBefore = time.Now().Add(time.Hour)
			return encode(t, response(t, i, o, assertion(o)))
		}},
		{"signed by another key", func(t *testing.T, i idp, id string) string {
			k, c := keyPair(t, "evil")
			o := good(id)
			return encode(t, response(t, idp{key: k, cert: c}, o, assertion(o)))
		}},
		{"tampered after signing", func(t *testing.T, i idp, id string) string {
			o := good(id)
			r := response(t, i, o, assertion(o))
			r.FindElement(".//NameID").SetText("admin")
			return encode(t, r)
		}},
		{"signature wrapping: evil assertion first, signed one hidden in extensions", func(t *testing.T, i idp, id string) string {
			o := good(id)
			signed := response(t, i, o, assertion(o)).FindElement("./Assertion")
			evil := assertion(o)
			evil.FindElement(".//NameID").SetText("admin")
			o.signAssert = false
			r := response(t, i, o, evil)
			ext := r.CreateElement("samlp:Extensions")
			ext.AddChild(signed.Copy())
			return encode(t, r)
		}},
		{"signature wrapping: signed assertion plus unsigned evil assertion", func(t *testing.T, i idp, id string) string {
			o := good(id)
			r := response(t, i, o, assertion(o))
			evil := assertion(o)
			evil.FindElement(".//NameID").SetText("admin")
			r.AddChild(evil)
			return encode(t, r)
		}},
		{"signature wrapping: evil assertion wrapping the signed one", func(t *testing.T, i idp, id string) string {
			o := good(id)
			signed := response(t, i, o, assertion(o)).FindElement("./Assertion")
			evil := assertion(o)
			evil.FindElement(".//NameID").SetText("admin")
			evil.FindElement("./Subject").AddChild(signed.Copy())
			o.signAssert = false
			return encode(t, response(t, i, o, evil))
		}},
		{"signed response, envelope altered", func(t *testing.T, i idp, id string) string {
			o := good(id)
			o.signAssert, o.signResp = false, true
			r := response(t, i, o, assertion(o))
			r.FindElement("./Assertion/Subject/NameID").SetText("admin")
			return encode(t, r)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, i, _ := setup(t)
			id := begin(t, p)
			got, err := p.Finish(ctx, c.build(t, i, id))
			if err == nil {
				t.Fatalf("accepted: %+v", got)
			}
			t.Logf("refused: %v", err)
		})
	}

	t.Run("replayed response", func(t *testing.T) {
		p, i, _ := setup(t)
		id := begin(t, p)
		o := good(id)
		enc := encode(t, response(t, i, o, assertion(o)))
		if _, err := p.Finish(ctx, enc); err != nil {
			t.Fatalf("first use: %v", err)
		}
		if _, err := p.Finish(ctx, enc); err == nil {
			t.Fatal("replayed response accepted")
		}
	})
}
