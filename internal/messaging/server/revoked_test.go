package server

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/usr-lex/swarmageddon/internal/messaging/revoked"
)

func TestRefusesRevokedCertificates(t *testing.T) {
	ca := newCA(t)
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	denyDir := filepath.Join(dir, "revoked")
	if err := os.Mkdir(denyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gone, goneKey := ca.issue(t, "hermes@acme", false)
	k, v, err := revoked.Entry(gone)
	if err != nil {
		t.Fatal(err)
	}
	write("revoked/"+k, []byte(v+"\n"))
	// An expired entry no longer denies anything (and isn't this cert anyway).
	write("revoked/abc", []byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)+" old@acme"))
	if err := os.Mkdir(filepath.Join(denyDir, "..data"), 0o700); err != nil { // kubelet's layout
		t.Fatal(err)
	}

	sc, sk := ca.issue(t, "messaging", true)
	tc, err := TLSConfigWithDenyList(write("tls.crt", sc), write("tls.key", sk), write("ca.crt", ca.pem), denyDir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(Identify(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := CallerFrom(r.Context())
		_, _ = io.WriteString(w, c.Address())
	}), false))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.pem)
	call := func(certPEM, keyPEM []byte) error {
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{pair}}}}
		res, err := hc.Get(srv.URL)
		if err != nil {
			return err
		}
		_ = res.Body.Close()
		return nil
	}
	if err := call(gone, goneKey); err == nil {
		t.Fatal("a revoked certificate connected")
	}
	// The same room with a new certificate is fine.
	if err := call(ca.issue(t, "hermes@acme", false)); err != nil {
		t.Fatalf("new certificate refused: %v", err)
	}
}
