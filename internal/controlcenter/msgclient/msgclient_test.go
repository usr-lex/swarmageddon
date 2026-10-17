package msgclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

type peers struct {
	messagingv1connect.UnimplementedMessagingServiceHandler
}

func (peers) ListPeers(context.Context, *connect.Request[messagingv1.ListPeersRequest]) (*connect.Response[messagingv1.ListPeersResponse], error) {
	return connect.NewResponse(&messagingv1.ListPeersResponse{}), nil
}

// The control center starts before the operator has issued its client
// certificate (an optional volume); calls work once the files appear.
func TestFilesAppearAfterNew(t *testing.T) {
	now := time.Now()
	ca, err := pki.NewCA("test CA", now)
	if err != nil {
		t.Fatal(err)
	}
	serving, err := ca.Issue(pki.Request{CommonName: "messaging", IPs: []net.IP{net.ParseIP("127.0.0.1")}, Usage: pki.Serving}, now)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Issue(pki.Request{CommonName: "control-center", Usage: pki.Client}, now)
	if err != nil {
		t.Fatal(err)
	}
	srvCert, err := tls.X509KeyPair(serving.CertPEM, serving.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	clients := x509.NewCertPool()
	clients.AddCert(ca.Cert)
	_, h := messagingv1connect.NewMessagingServiceHandler(peers{})
	ts := httptest.NewUnstartedServer(h)
	ts.EnableHTTP2 = true
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clients}
	ts.StartTLS()
	defer ts.Close()

	dir := t.TempDir()
	certFile, keyFile, caFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), filepath.Join(dir, "ca.crt")
	c, err := New(ts.URL, certFile, keyFile, caFile)
	if err != nil {
		t.Fatalf("New before the files exist: %v", err)
	}
	call := func() error {
		_, err := c.ListPeers(context.Background(), connect.NewRequest(&messagingv1.ListPeersRequest{}))
		return err
	}
	if err := call(); err == nil {
		t.Fatal("call succeeded without a client certificate")
	}
	for name, data := range map[string][]byte{certFile: client.CertPEM, keyFile: client.KeyPEM, caFile: ca.CertPEM} {
		if err := os.WriteFile(name, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := call(); err != nil {
		t.Fatalf("call after the files appeared: %v", err)
	}

	// A server the CA didn't sign is refused.
	other := httptest.NewTLSServer(http.NotFoundHandler())
	defer other.Close()
	c2, _ := New(other.URL, certFile, keyFile, caFile)
	if _, err := c2.ListPeers(context.Background(), connect.NewRequest(&messagingv1.ListPeersRequest{})); err == nil {
		t.Fatal("a server outside the CA was trusted")
	}
}
