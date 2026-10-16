package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"

	"github.com/usr-lex/swarmageddon/internal/messaging/policy"
)

// Headers.
const (
	// HeaderOnBehalfOf lets the control center act for a person, or for the
	// forge system sender: <handle>@<workspace>.
	HeaderOnBehalfOf = "x-swarm-on-behalf-of"
	// HeaderDevIdentity carries the caller's identity in --dev-insecure mode
	// instead of a client certificate's CN.
	HeaderDevIdentity = "x-swarm-dev-identity"
	// ControlCenterCN is the control center's certificate CN.
	ControlCenterCN = "control-center"
)

// Caller is who is calling, from the connection's client certificate.
type Caller struct {
	// ControlCenter is the control center's principal.
	ControlCenter bool
	// Workspace and Name are the room, or the party the control center acts
	// for; empty for the control center acting as itself.
	Workspace, Name string
	Kind            policy.Kind
}

// Address is name@workspace, or "control-center".
func (c Caller) Address() string {
	if c.Name == "" {
		return ControlCenterCN
	}
	return policy.Address(c.Name, c.Workspace)
}

type callerKey struct{}

// CallerFrom returns the caller the identity middleware stored.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// Identify wraps h, reading the caller's identity from the verified client
// certificate (or, if dev, from the x-swarm-dev-identity header) and the
// x-swarm-on-behalf-of header. Unidentified calls get Unauthenticated.
func Identify(h http.Handler, dev bool) http.Handler {
	ew := connect.NewErrorWriter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var cn string
		switch {
		case dev:
			cn = r.Header.Get(HeaderDevIdentity)
		case r.TLS != nil && len(r.TLS.VerifiedChains) > 0:
			cn = r.TLS.VerifiedChains[0][0].Subject.CommonName
		}
		c, err := parseCaller(cn, r.Header.Get(HeaderOnBehalfOf))
		if err != nil {
			_ = ew.Write(w, r, err)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	})
}

func parseCaller(cn, onBehalf string) (Caller, error) {
	if cn == "" {
		return Caller{}, connect.NewError(connect.CodeUnauthenticated, errors.New("no client certificate"))
	}
	if cn == ControlCenterCN {
		c := Caller{ControlCenter: true}
		if onBehalf == "" {
			return c, nil
		}
		name, ws, err := policy.SplitAddress(onBehalf)
		if err != nil {
			return Caller{}, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", HeaderOnBehalfOf, err))
		}
		c.Workspace, c.Name, c.Kind = ws, name, policy.Human
		if policy.IsSystem(name) {
			c.Kind = policy.System
		}
		return c, nil
	}
	if onBehalf != "" {
		return Caller{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("only the control center may use %s", HeaderOnBehalfOf))
	}
	name, ws, err := policy.SplitAddress(cn)
	if err != nil || policy.IsSystem(name) {
		return Caller{}, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("certificate subject %q is not a room (name@workspace)", cn))
	}
	return Caller{Workspace: ws, Name: name, Kind: policy.Agent}, nil
}
