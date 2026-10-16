package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func mustCA(t *testing.T) *CA {
	t.Helper()
	ca, err := NewCA("test CA", t0)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestServingCertVerifies(t *testing.T) {
	ca := mustCA(t)
	leaf, err := ca.Issue(Request{CommonName: "messaging", DNSNames: []string{"messaging.swarmageddon-platform.svc"}, IPs: []net.IP{net.ParseIP("10.0.0.5")}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ParseCert(leaf.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	for _, name := range []string{"messaging.swarmageddon-platform.svc", "10.0.0.5"} {
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: pool, CurrentTime: t0.Add(time.Hour), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Errorf("verify %s: %v", name, err)
		}
	}
	if got := cert.NotAfter.Sub(t0); got != ServingLifetime {
		t.Errorf("lifetime %v, want %v", got, ServingLifetime)
	}
	if _, err := tls.X509KeyPair(leaf.CertPEM, leaf.KeyPEM); err != nil {
		t.Errorf("key pair: %v", err)
	}
}

func TestClientCert(t *testing.T) {
	ca := mustCA(t)
	uri := "spiffe://swarmageddon/ws/acme/room/athena"
	leaf, err := ca.Issue(Request{CommonName: "athena@acme", URIs: []string{uri}, Usage: Client}, t0)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := ParseCert(leaf.CertPEM)
	if cert.Subject.CommonName != "athena@acme" || len(cert.URIs) != 1 || cert.URIs[0].String() != uri {
		t.Fatalf("subject %q uris %v", cert.Subject.CommonName, cert.URIs)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: t0, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("client cert verified as a server cert")
	}
	if got := cert.NotAfter.Sub(t0); got != ClientLifetime {
		t.Errorf("lifetime %v", got)
	}
}

func TestNeedsRenewal(t *testing.T) {
	ca := mustCA(t)
	req := Request{CommonName: "athena@acme", URIs: []string{"spiffe://swarmageddon/ws/acme/room/athena"}, Usage: Client}
	leaf, _ := ca.Issue(req, t0)
	cases := []struct {
		name string
		cert []byte
		req  Request
		now  time.Time
		want bool
	}{
		{"fresh", leaf.CertPEM, req, t0.Add(time.Hour), false},
		{"just before 2/3", leaf.CertPEM, req, t0.Add(19 * 24 * time.Hour), false},
		{"after 2/3", leaf.CertPEM, req, t0.Add(21 * 24 * time.Hour), true},
		{"garbage", []byte("nope"), req, t0, true},
		{"empty", nil, req, t0, true},
		{"different CN", leaf.CertPEM, Request{CommonName: "other@acme", URIs: req.URIs}, t0, true},
		{"different SANs", leaf.CertPEM, Request{CommonName: req.CommonName, URIs: []string{"spiffe://x"}}, t0, true},
	}
	for _, c := range cases {
		if got := ca.NeedsRenewal(c.cert, c.req, c.now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	other := mustCA(t)
	if !other.NeedsRenewal(leaf.CertPEM, req, t0) {
		t.Error("cert from another CA should need renewal")
	}
}

func TestParseCARoundTrip(t *testing.T) {
	ca := mustCA(t)
	again, err := ParseCA(ca.CertPEM, ca.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if again.Fingerprint() != ca.Fingerprint() {
		t.Fatal("fingerprint changed")
	}
	other := mustCA(t)
	if _, err := ParseCA(ca.CertPEM, other.KeyPEM); err == nil {
		t.Fatal("mismatched key accepted")
	}
}

func TestFingerprintFormat(t *testing.T) {
	fp := mustCA(t).Fingerprint()
	parts := strings.Split(fp, ":")
	if len(parts) != 32 {
		t.Fatalf("fingerprint %q", fp)
	}
	if strings.ToUpper(fp) != fp {
		t.Fatalf("not upper case: %q", fp)
	}
}

func TestStoreCreatesOnceAndReloads(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	s1 := &Store{Client: c, Namespace: "sys", Now: func() time.Time { return t0 }}
	ca1, err := s1.CA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s2 := &Store{Client: c, Namespace: "sys"}
	ca2, err := s2.CA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ca1.Fingerprint() != ca2.Fingerprint() {
		t.Fatal("second store made a new CA")
	}
}

func TestEnsureTLSSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()
	ca := mustCA(t)
	key := types.NamespacedName{Namespace: "sys", Name: "control-center-tls"}
	req := Request{CommonName: "control-center", DNSNames: []string{"control-center"}}
	issued, err := EnsureTLSSecret(ctx, c, ca, key, req, t0, nil)
	if err != nil || !issued {
		t.Fatalf("first: issued=%v err=%v", issued, err)
	}
	var sec corev1.Secret
	_ = c.Get(ctx, key, &sec)
	first := string(sec.Data[TLSCertKey])
	sec.Data["other"] = []byte("kept")
	_ = c.Update(ctx, &sec)

	issued, err = EnsureTLSSecret(ctx, c, ca, key, req, t0.Add(time.Hour), nil)
	if err != nil || issued {
		t.Fatalf("second: issued=%v err=%v", issued, err)
	}
	issued, err = EnsureTLSSecret(ctx, c, ca, key, req, t0.Add(80*24*time.Hour), nil)
	if err != nil || !issued {
		t.Fatalf("renewal: issued=%v err=%v", issued, err)
	}
	_ = c.Get(ctx, key, &sec)
	if string(sec.Data[TLSCertKey]) == first || string(sec.Data["other"]) != "kept" || len(sec.Data[CACertKey]) == 0 {
		t.Fatal("renewal did not replace the cert or lost other keys")
	}
}
