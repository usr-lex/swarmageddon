// Package msgclient is the control center's client for the messaging service
// (docs/design.md §7, ADR 0003): Connect's gRPC protocol over HTTP/2 with the
// control-center client certificate. The certificate, key and CA files are
// read on every handshake: the control center starts before the operator has
// issued them (they are optional volumes), and a rotated Secret takes effect
// without a restart.
package msgclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
)

// OnBehalfHeader names the person the control center acts for.
const OnBehalfHeader = "x-swarm-on-behalf-of"

// Client wraps the generated client.
type Client struct {
	messagingv1connect.MessagingServiceClient
}

// New returns a client of the messaging service at baseURL that
// authenticates with the client certificate in certFile/keyFile and trusts
// the CA in caFile. Missing files fail the calls made until they appear,
// not New.
func New(baseURL, certFile, keyFile, caFile string) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		// The default verification with roots read now rather than at
		// startup (crypto/tls's documented VerifyConnection pattern).
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error { return verifyPeer(cs, caFile) }
	}
	if certFile != "" {
		tlsCfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return nil, fmt.Errorf("client certificate: %w", err)
			}
			return &c, nil
		}
	}
	tr := &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second}
	hc := &http.Client{Transport: tr}
	return &Client{messagingv1connect.NewMessagingServiceClient(hc, baseURL, connect.WithGRPC())}, nil
}

// verifyPeer checks the server's chain and name against the CA in caFile.
func verifyPeer(cs tls.ConnectionState, caFile string) error {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return fmt.Errorf("messaging CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return errors.New("no certificates in CA file")
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("messaging presented no certificate")
	}
	opts := x509.VerifyOptions{DNSName: cs.ServerName, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err = cs.PeerCertificates[0].Verify(opts)
	return err
}

// NewWith wraps an existing client (tests).
func NewWith(c messagingv1connect.MessagingServiceClient) *Client { return &Client{c} }

// SendAs sends a message as a person or system sender (address name@workspace).
func (c *Client) SendAs(ctx context.Context, as, id, to, body, replyTo string) (*messagingv1.SendResponse, error) {
	if id == "" {
		id = ulid.Make().String()
	}
	req := connect.NewRequest(&messagingv1.SendRequest{Id: id, To: to, Body: body, ReplyTo: replyTo})
	req.Header().Set(OnBehalfHeader, as)
	res, err := c.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// Loop runs fn until ctx ends, restarting it with backoff when it returns.
// It is how the long-lived Watch* streams are kept open.
func Loop(ctx context.Context, name string, fn func(ctx context.Context) error) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := fn(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		slog.Warn("messaging stream ended; reconnecting", "stream", name, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
