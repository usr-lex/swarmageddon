package msgclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/supervisor/msgclient/msgfake"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestDedup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d", "seen")
	d, err := LoadDedup(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := d.Add(id); err != nil {
			t.Fatal(err)
		}
	}
	if d.Seen("a") || !d.Seen("d") {
		t.Error("window")
	}
	d2, err := LoadDedup(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !d2.Seen("b") || !d2.Seen("d") || d2.Seen("a") {
		t.Error("not persisted")
	}
}

func TestSubscribeAcksAfterDeliveryAndDedups(t *testing.T) {
	fake := msgfake.New()
	_, h := messagingv1connect.NewMessagingServiceHandler(fake)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client, err := New(srv.URL, TLSFiles{})
	if err != nil {
		t.Fatal(err)
	}
	dedup, _ := LoadDedup(filepath.Join(t.TempDir(), "seen"), 100)
	var mu sync.Mutex
	var got []string
	fails := 1
	handler := func(_ context.Context, d *messagingv1.Delivery) error {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return errors.New("agent restarting")
		}
		got = append(got, d.GetMessage().GetBody())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go Subscribe(ctx, client, dedup, handler, quiet)
	fake.PushMessage("m1", "hermes@acme", "one")
	fake.PushMessage("m1", "hermes@acme", "one again") // duplicate id
	fake.PushMessage("m2", "hermes@acme", "two")
	ok := fake.WaitFor(ctx, func() bool { _, acks, _, _, _, _ := fake.Snapshot(); return len(acks) == 3 })
	if !ok {
		t.Fatal("not all acked")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("delivered %q", got)
	}
}

// pki writes a CA and a leaf signed by it; returns file paths.
func pki(t *testing.T, dir, name string, server bool) (TLSFiles, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name + "-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	files := TLSFiles{Cert: filepath.Join(dir, name+".crt"), Key: filepath.Join(dir, name+".key"), CA: filepath.Join(dir, name+"-ca.crt")}
	writeLeaf(t, files, ca, caKey, name, server)
	os.WriteFile(files.CA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)
	return files, ca, caKey
}

func writeLeaf(t *testing.T, files TLSFiles, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(files.Cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(files.Key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

func TestMTLSAndReload(t *testing.T) {
	dir := t.TempDir()
	srvFiles, _, _ := pki(t, dir, "server", true)
	cliFiles, cliCA, cliCAKey := pki(t, dir, "client", false)
	// The client trusts the server's CA.
	room := TLSFiles{Cert: cliFiles.Cert, Key: cliFiles.Key, CA: srvFiles.CA}

	var mu sync.Mutex
	var seenCN []string
	fake := msgfake.New()
	_, h := messagingv1connect.NewMessagingServiceHandler(fake)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenCN = append(seenCN, r.TLS.PeerCertificates[0].Subject.CommonName)
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	pool := x509.NewCertPool()
	pool.AddCert(cliCA)
	sc, _ := tls.LoadX509KeyPair(srvFiles.Cert, srvFiles.Key)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{sc}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	defer srv.Close()

	c, err := New(srv.URL, room)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.ReportPresence(ctx, connect.NewRequest(&messagingv1.ReportPresenceRequest{Presence: messagingv1.Presence_PRESENCE_IDLE})); err != nil {
		t.Fatal(err)
	}
	// Rotate the client certificate in place; new connections use it.
	time.Sleep(10 * time.Millisecond)
	writeLeaf(t, room, cliCA, cliCAKey, "athena@acme", false)
	srv.CloseClientConnections()
	if _, err := c.ReportPresence(ctx, connect.NewRequest(&messagingv1.ReportPresenceRequest{})); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seenCN[0] != "client" || seenCN[len(seenCN)-1] != "athena@acme" {
		t.Errorf("CNs %v", seenCN)
	}

	// A server not signed by the trusted CA is refused.
	other, _, _ := pki(t, t.TempDir(), "other", false)
	bad, _ := New(srv.URL, TLSFiles{Cert: cliFiles.Cert, Key: cliFiles.Key, CA: other.CA})
	if _, err := bad.ReportPresence(ctx, connect.NewRequest(&messagingv1.ReportPresenceRequest{})); err == nil {
		t.Error("untrusted server accepted")
	}
}
