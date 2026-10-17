//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"connectrpc.com/connect"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1/controlcenterv1connect"
)

// cc is the control center as the web UI uses it: HTTPS that trusts only the
// platform CA (checked against the fingerprint the install printed), a
// cookie jar for the session, and the Connect clients of
// proto/swarm/controlcenter/v1.
type cc struct {
	base string
	ca   *x509.CertPool

	setup       controlcenterv1connect.SetupServiceClient
	auth        controlcenterv1connect.AuthServiceClient
	platform    controlcenterv1connect.PlatformServiceClient
	workspaces  controlcenterv1connect.WorkspaceServiceClient
	rooms       controlcenterv1connect.RoomServiceClient
	inbox       controlcenterv1connect.InboxServiceClient
	channels    controlcenterv1connect.ChannelServiceClient
	audit       controlcenterv1connect.AuditServiceClient
	observatory controlcenterv1connect.ObservatoryServiceClient
}

// newCC returns a client with an empty cookie jar: a fresh browser.
func newCC(base string, ca *x509.CertPool) *cc {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: ca, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
	}}
	api := strings.TrimRight(base, "/") + "/api"
	return &cc{
		base: base, ca: ca,
		setup:       controlcenterv1connect.NewSetupServiceClient(hc, api),
		auth:        controlcenterv1connect.NewAuthServiceClient(hc, api),
		platform:    controlcenterv1connect.NewPlatformServiceClient(hc, api),
		workspaces:  controlcenterv1connect.NewWorkspaceServiceClient(hc, api),
		rooms:       controlcenterv1connect.NewRoomServiceClient(hc, api),
		inbox:       controlcenterv1connect.NewInboxServiceClient(hc, api),
		channels:    controlcenterv1connect.NewChannelServiceClient(hc, api),
		audit:       controlcenterv1connect.NewAuditServiceClient(hc, api),
		observatory: controlcenterv1connect.NewObservatoryServiceClient(hc, api),
	}
}

// call makes one unary call with a deadline and returns the response message.
func call[Req, Res any](f func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), req *Req) (*Res, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := f(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// pinCA parses the platform CA and checks it against the fingerprint the
// Platform status (and the welcome output) shows: SHA-256 of the DER.
func pinCA(caPEM []byte, fingerprint string) (*x509.CertPool, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return nil, errors.New("the CA bundle holds no PEM block")
	}
	sum := sha256.Sum256(block.Bytes)
	norm := func(s string) string {
		s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "sha256:")
		return strings.ReplaceAll(s, ":", "")
	}
	if got := hex.EncodeToString(sum[:]); got != norm(fingerprint) {
		return nil, fmt.Errorf("the CA's SHA-256 is %s, but the Platform status shows %s", got, fingerprint)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool, nil
}

func ref(room string) *ccv1.RoomRef { return &ccv1.RoomRef{Workspace: ws, Name: room} }

// cond finds a condition by type.
func cond(cs []*ccv1.Condition, typ string) *ccv1.Condition {
	for _, c := range cs {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

// notTrue lists the conditions of types that aren't True, with why.
func notTrue(cs []*ccv1.Condition, types ...string) error {
	var bad []string
	for _, typ := range types {
		c := cond(cs, typ)
		switch {
		case c == nil:
			bad = append(bad, typ+": not reported")
		case c.GetStatus() != "True":
			bad = append(bad, fmt.Sprintf("%s: %s %s: %s", typ, c.GetStatus(), c.GetReason(), c.GetMessage()))
		}
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}
