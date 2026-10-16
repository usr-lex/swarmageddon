package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) *testCA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a PEM cert and key signed by the CA.
func (ca *testCA) issue(t *testing.T, cn string, server bool) (certPEM, keyPEM []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func TestMTLSIdentity(t *testing.T) {
	ca, rogue := newCA(t), newCA(t)
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sc, sk := ca.issue(t, "messaging", true)
	tc, err := TLSConfig(write("tls.crt", sc), write("tls.key", sk), write("ca.crt", ca.pem))
	if err != nil {
		t.Fatal(err)
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := CallerFrom(r.Context())
		io.WriteString(w, c.Address()+"|"+string(c.Kind))
	})
	srv := httptest.NewUnstartedServer(Identify(echo, false))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.pem)
	call := func(issuer *testCA, cn, onBehalf string) (int, string, error) {
		cfg := &tls.Config{RootCAs: roots}
		if issuer != nil {
			c, k := issuer.issue(t, cn, false)
			pair, err := tls.X509KeyPair(c, k)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Certificates = []tls.Certificate{pair}
		}
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		req, _ := http.NewRequest("POST", srv.URL, nil)
		req.Header.Set("Content-Type", "application/json")
		if onBehalf != "" {
			req.Header.Set(HeaderOnBehalfOf, onBehalf)
		}
		req.Header.Set(HeaderDevIdentity, "control-center") // ignored without --dev-insecure
		res, err := hc.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), nil
	}

	tests := []struct {
		name          string
		issuer        *testCA
		cn, onBehalf  string
		status        int
		body          string
		handshakeFail bool
	}{
		{name: "room", issuer: ca, cn: "hermes@acme", status: 200, body: "hermes@acme|agent"},
		{name: "control center", issuer: ca, cn: "control-center", status: 200, body: "control-center|"},
		{name: "control center for a person", issuer: ca, cn: "control-center", onBehalf: "alice@acme", status: 200, body: "alice@acme|human"},
		{name: "control center as forge", issuer: ca, cn: "control-center", onBehalf: "forge@acme", status: 200, body: "forge@acme|system"},
		{name: "room can't act for others", issuer: ca, cn: "hermes@acme", onBehalf: "alice@acme", status: 403},
		{name: "room can't be a system address", issuer: ca, cn: "forge@acme", status: 401},
		{name: "bad subject", issuer: ca, cn: "someone", status: 401},
		{name: "untrusted CA", issuer: rogue, cn: "control-center", handshakeFail: true},
		{name: "no client certificate", handshakeFail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body, err := call(tc.issuer, tc.cn, tc.onBehalf)
			if tc.handshakeFail {
				if err == nil {
					t.Fatalf("connected: %d %s", status, body)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if status != tc.status || (tc.body != "" && body != tc.body) {
				t.Fatalf("got %d %q; want %d %q", status, body, tc.status, tc.body)
			}
		})
	}
}
