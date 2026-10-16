// Package pki is swarmageddon's internal certificate authority (docs/design.md
// §3.2, §7.5). The CA is an ECDSA P-256 key and a self-signed certificate,
// kept in Secret platform-ca in the system namespace. It issues serving
// certificates (DNS and IP SANs) and client certificates (a CN and a URI SAN),
// and says when a certificate is due for renewal (at 2/3 of its lifetime).
package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Default lifetimes.
const (
	CALifetime      = 10 * 365 * 24 * time.Hour
	ServingLifetime = 90 * 24 * time.Hour
	ClientLifetime  = 30 * 24 * time.Hour
	// backdate protects against small clock skew between nodes.
	backdate = 5 * time.Minute
)

// CA is a loaded certificate authority.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
}

// Usage is what a leaf certificate is for.
type Usage int

const (
	// Serving is a TLS server certificate.
	Serving Usage = iota
	// Client is a TLS client certificate.
	Client
	// ServingAndClient is both, e.g. for messaging's NATS routes.
	ServingAndClient
)

// Request describes a leaf certificate.
type Request struct {
	CommonName string
	DNSNames   []string
	IPs        []net.IP
	URIs       []string
	Usage      Usage
	// Lifetime; zero picks ServingLifetime or ClientLifetime.
	Lifetime time.Duration
}

// Leaf is an issued certificate and its key, PEM-encoded.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA creates a self-signed CA valid from now for CALifetime.
func NewCA(commonName string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"swarmageddon"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	return ParseCA(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM)
}

// ParseCA loads a CA from PEM.
func ParseCA(certPEM, keyPEM []byte) (*CA, error) {
	cert, err := ParseCert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("ca certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("ca certificate is not a CA")
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("ca key: no PEM block")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("ca key does not match certificate")
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

// Issue signs a new leaf certificate for req, valid from now.
func (ca *CA) Issue(req Request, now time.Time) (Leaf, error) {
	lifetime := req.Lifetime
	if lifetime == 0 {
		lifetime = ServingLifetime
		if req.Usage == Client {
			lifetime = ClientLifetime
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Leaf{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return Leaf{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: req.CommonName},
		NotBefore:    now.Add(-backdate),
		NotAfter:     now.Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     req.DNSNames,
		IPAddresses:  req.IPs,
	}
	if tmpl.NotAfter.After(ca.Cert.NotAfter) {
		tmpl.NotAfter = ca.Cert.NotAfter
	}
	switch req.Usage {
	case Serving:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	case Client:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	case ServingAndClient:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	for _, u := range req.URIs {
		parsed, err := url.Parse(u)
		if err != nil {
			return Leaf{}, fmt.Errorf("uri SAN %q: %w", u, err)
		}
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return Leaf{}, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return Leaf{}, err
	}
	return Leaf{CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), KeyPEM: keyPEM}, nil
}

// NeedsRenewal reports whether certPEM should be replaced: it can't be
// parsed, wasn't signed by this CA, doesn't match req, or has passed 2/3 of
// its lifetime.
func (ca *CA) NeedsRenewal(certPEM []byte, req Request, now time.Time) bool {
	cert, err := ParseCert(certPEM)
	if err != nil {
		return true
	}
	if err := cert.CheckSignatureFrom(ca.Cert); err != nil {
		return true
	}
	if !matches(cert, req) {
		return true
	}
	return now.After(RenewAt(cert))
}

// RenewAt is when a certificate passes 2/3 of its lifetime.
func RenewAt(cert *x509.Certificate) time.Time {
	life := cert.NotAfter.Sub(cert.NotBefore)
	return cert.NotBefore.Add(life * 2 / 3)
}

func matches(cert *x509.Certificate, req Request) bool {
	if cert.Subject.CommonName != req.CommonName {
		return false
	}
	if !sameStrings(cert.DNSNames, req.DNSNames) {
		return false
	}
	var ips, wantIPs []string
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	for _, ip := range req.IPs {
		wantIPs = append(wantIPs, ip.String())
	}
	if !sameStrings(ips, wantIPs) {
		return false
	}
	var uris []string
	for _, u := range cert.URIs {
		uris = append(uris, u.String())
	}
	return sameStrings(uris, req.URIs)
}

func sameStrings(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// ParseCert decodes the first certificate in a PEM bundle.
func ParseCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// Fingerprint is the SHA-256 of a certificate's DER, as colon-separated
// upper-case hex, the form browsers show.
func Fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("no PEM block")
	}
	sum := sha256.Sum256(block.Bytes)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b bytes.Buffer
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(h[i : i+2])
	}
	return b.String(), nil
}

// IssueClientCert issues a client certificate with CN cn and URI SAN uriSAN,
// valid from now for ttl (deps.PKI).
func (ca *CA) IssueClientCert(cn, uriSAN string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	var uris []string
	if uriSAN != "" {
		uris = []string{uriSAN}
	}
	leaf, err := ca.Issue(Request{CommonName: cn, URIs: uris, Usage: Client, Lifetime: ttl}, time.Now())
	return leaf.CertPEM, leaf.KeyPEM, err
}

// CABundle is the CA certificate in PEM (deps.PKI).
func (ca *CA) CABundle() []byte { return ca.CertPEM }

// Fingerprint of the CA certificate.
func (ca *CA) Fingerprint() string {
	fp, _ := Fingerprint(ca.CertPEM)
	return fp
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}
