// Package deps holds what the operator's controllers share. cmd/operator
// builds one Deps and hands it to every controller's
// SetupWithManager(mgr, deps.Deps).
package deps

import (
	"context"
	"errors"
	"time"

	"k8s.io/utils/clock"

	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/gateway"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// PKI issues certificates from the platform's internal CA. *pki.CA
// implements it.
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
	// Forge returns an admin client for Forgejo, or an error (wrapping
	// ErrNotReady) while it isn't ready.
	Forge func(ctx context.Context) (*forgejo.Client, error)
	// Gateway returns a master-key client for LiteLLM, or an error (wrapping
	// ErrNotReady) while it isn't ready.
	Gateway func(ctx context.Context) (*gateway.Client, error)
	// PlatformTier is the isolation tier rooms run with: userns, sysbox or privileged.
	PlatformTier func(ctx context.Context) (string, error)

	// Clock; nil means the real clock.
	Clock clock.PassiveClock
	// CAStore loads (or creates) the CA behind PKI; the platform controller uses it.
	CAStore *pki.Store
	// Pinned is every image this operator deploys.
	Pinned images.Images
	// Version of the operator.
	Version string
}

// ErrNotReady means a platform service isn't deployed or reachable yet;
// controllers set a condition and retry later.
var ErrNotReady = errors.New("platform service not ready")

// Now is the current time from d.Clock.
func (d Deps) Now() time.Time {
	if d.Clock != nil {
		return d.Clock.Now()
	}
	return time.Now()
}

// ImageRefs adapts pinned images to the Images interface.
func ImageRefs(i images.Images) Images { return imageRefs{room: i.Room, runner: i.Runner} }

type imageRefs struct{ room, runner string }

func (i imageRefs) Room() string   { return i.room }
func (i imageRefs) Runner() string { return i.runner }
