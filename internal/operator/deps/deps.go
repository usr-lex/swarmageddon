// Package deps holds what the operator's controllers share.
//
// Provisional: the lead reconciles with the platform agent's version. The
// room and runnerpool controllers use only the fields below.
package deps

import (
	"context"
	"time"

	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/gateway"
)

// PKI issues certificates from the platform's internal CA.
type PKI interface {
	IssueClientCert(cn string, uriSAN string, ttl time.Duration) (certPEM, keyPEM []byte, err error)
	CABundle() []byte
}

// Images names the images this operator version runs.
type Images interface {
	Room() string
	Runner() string
}

// Deps are the controllers' shared dependencies.
type Deps struct {
	PKI    PKI
	Images Images
	// Forge returns an admin client for Forgejo, or an error while it isn't ready.
	Forge func(ctx context.Context) (*forgejo.Client, error)
	// Gateway returns a master-key client for LiteLLM, or an error while it isn't ready.
	Gateway func(ctx context.Context) (*gateway.Client, error)
	// PlatformTier is the isolation tier rooms run with: userns, sysbox or privileged.
	PlatformTier func(ctx context.Context) (string, error)
}
