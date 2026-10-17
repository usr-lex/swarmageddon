// Package telemetry reports errors, logs and traces to Sentry
// (docs/conventions.md, "Error reporting"). It is off unless SENTRY_DSN is
// set: until Init finds one, every function here does nothing or returns
// its argument unchanged.
//
// Whatever is sent passes through the redact registry first (design §11.4):
// messages, exceptions, breadcrumbs, log records, span names and data, and
// request URLs and headers (scrub.go). Credential headers are dropped, and
// request bodies are never sent.
//
// Rooms never report: the room supervisor and the swarm tool don't use this
// package. A room holds only its own identity (design §11.4), so a DSN there
// would be a secret shared by every room, and rooms usually have no internet
// access to reach Sentry with anyway.
package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	sentryhttpclient "github.com/getsentry/sentry-go/httpclient"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/version"
)

// Environment variables Init reads.
const (
	EnvDSN              = "SENTRY_DSN"
	EnvEnvironment      = "SENTRY_ENVIRONMENT"
	EnvTracesSampleRate = "SENTRY_TRACES_SAMPLE_RATE"
)

// DefaultTracesSampleRate is the share of requests traced when
// SENTRY_TRACES_SAMPLE_RATE is unset.
const DefaultTracesSampleRate = 0.1

// FlushTimeout bounds how long flush waits for queued events at shutdown.
const FlushTimeout = 2 * time.Second

// Config is the Sentry settings.
type Config struct {
	DSN         string
	Environment string
	// TracesSampleRate is between 0 and 1; 0 turns tracing off.
	TracesSampleRate float64
}

// FromEnv reads the settings from SENTRY_DSN, SENTRY_ENVIRONMENT and
// SENTRY_TRACES_SAMPLE_RATE.
func FromEnv() (Config, error) {
	c := Config{DSN: os.Getenv(EnvDSN), Environment: os.Getenv(EnvEnvironment), TracesSampleRate: DefaultTracesSampleRate}
	if s := os.Getenv(EnvTracesSampleRate); s != "" {
		r, err := strconv.ParseFloat(s, 64)
		if err != nil || r < 0 || r > 1 {
			return Config{}, fmt.Errorf("%s=%q: want a number from 0 to 1", EnvTracesSampleRate, s)
		}
		c.TracesSampleRate = r
	}
	return c, nil
}

var (
	enabled atomic.Bool
	current Config

	instrumentOnce sync.Once
)

// Enabled reports whether Init turned reporting on.
func Enabled() bool { return enabled.Load() }

// Current returns the settings reporting runs with, or the zero Config when
// it is off.
func Current() Config {
	if !Enabled() {
		return Config{}
	}
	return current
}

// Init starts reporting for component (the binary's name, sent as tag
// "component") when SENTRY_DSN is set. Call it before building loggers and
// servers, and call flush before the process exits. Without a DSN, or when
// the settings are invalid (err), reporting stays off and flush does nothing.
func Init(component string) (flush func(), err error) {
	c, err := FromEnv()
	if err != nil || c.DSN == "" {
		return func() {}, err
	}
	if err := start(component, c, nil); err != nil {
		return func() {}, err
	}
	if c.TracesSampleRate > 0 {
		// Requests through http.DefaultTransport (a zero http.Client,
		// httputil.ReverseProxy) get a span in the caller's transaction.
		instrumentOnce.Do(func() { http.DefaultTransport = sentryhttpclient.NewSentryRoundTripper(http.DefaultTransport) })
	}
	return func() { sentry.Flush(FlushTimeout) }, nil
}

// propagationTargets are the URLs outgoing requests carry trace headers to:
// our own services, which continue traces. LiteLLM, Forgejo and the
// internet don't get them.
var propagationTargets = []string{
	"://messaging." + v1alpha1.PlatformNamespace + ".svc",
	"://gateway." + v1alpha1.PlatformNamespace + ".svc",
	"://control-center." + v1alpha1.SystemNamespace + ".svc",
}

// ignoreErrors are routine errors that aren't worth an event: a controller
// retries an update that lost a race, and httputil.ReverseProxy (the
// warden) panics with http.ErrAbortHandler when a client goes away.
var ignoreErrors = []string{
	"the object has been modified; please apply your changes to the latest version",
	"^" + http.ErrAbortHandler.Error() + "$",
}

// start initialises the SDK; tr nil means Sentry's own HTTP transport.
func start(component string, c Config, tr sentry.Transport) error {
	server := os.Getenv("POD_NAME")
	if server == "" {
		server, _ = os.Hostname()
	}
	rate := c.TracesSampleRate
	err := sentry.Init(sentry.ClientOptions{
		Dsn:           c.DSN,
		Environment:   c.Environment,
		Release:       "swarmageddon@" + version.Version,
		ServerName:    server,
		Tags:          map[string]string{"component": component},
		EnableTracing: rate > 0,
		// A fixed rate, whatever a caller's sentry-trace header says: rooms
		// and the internet reach some of these servers, and mustn't be able
		// to make us trace (and send) every request.
		TracesSampler: func(sentry.SamplingContext) float64 { return rate },
		// Continue only traces that carry our Sentry organization.
		StrictTraceContinuation: true,
		TracePropagationTargets: propagationTargets,
		DataCollection:          dataCollection(),
		BeforeSend:              scrubEvent,
		BeforeSendTransaction:   scrubEvent,
		BeforeSendLog:           scrubLog,
		BeforeBreadcrumb:        scrubBreadcrumb,
		IgnoreErrors:            ignoreErrors,
		Transport:               tr,
	})
	if err != nil {
		return fmt.Errorf("sentry: %w", err)
	}
	redact.Add(c.DSN)
	// Sentry Logs carry scope attributes, not tags.
	sentry.CurrentHub().Scope().SetAttributes(attribute.String("component", component))
	current = c
	enabled.Store(true)
	return nil
}

// dataCollection keeps users, cookies and bodies out of what the SDK
// collects, and filters header and query values whose names look
// sensitive: besides the SDK's own list (auth, key, token, secret,
// session...), ours and, as the SDK's private default does, client
// addresses and proxies' user headers. scrubRequest drops credential
// headers outright.
func dataCollection() *sentry.DataCollection {
	deny := &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionDenyList,
		Terms: []string{"x-swarm-", "cookie", "forwarded", "-ip", "remote-", "via", "-user"}}
	return &sentry.DataCollection{
		UserInfo:    sentry.Set(false),
		Cookies:     &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionOff},
		HTTPHeaders: &sentry.HeaderCollectionConfig{Request: deny, Response: deny},
		HTTPBodies:  []sentry.BodyType{},
		QueryParams: &sentry.KeyValueCollectionBehavior{Mode: sentry.CollectionDenyList},
	}
}

// BrowserConfig is what the web UI starts Sentry with. The control center
// serves it at /client-config.json.
type BrowserConfig struct {
	DSN              string  `json:"dsn"`
	Environment      string  `json:"environment,omitempty"`
	Release          string  `json:"release"`
	TracesSampleRate float64 `json:"tracesSampleRate"`
}

// Browser returns the web UI's settings, or nil when reporting is off.
func Browser() *BrowserConfig {
	c := Current()
	if c.DSN == "" {
		return nil
	}
	return &BrowserConfig{DSN: c.DSN, Environment: c.Environment, Release: "swarmageddon@" + version.Version, TracesSampleRate: c.TracesSampleRate}
}

// RecoverPanic reports a recovered panic. It fits
// k8s.io/apimachinery/pkg/util/runtime.PanicHandlers, which controller-runtime
// calls when a reconcile panics.
func RecoverPanic(ctx context.Context, r any) {
	if Enabled() {
		hub(ctx).RecoverWithContext(ctx, r)
	}
}

// hub is the request's hub (sentryhttp puts one in the context), or the
// process's.
func hub(ctx context.Context) *sentry.Hub {
	if ctx != nil {
		if h := sentry.GetHubFromContext(ctx); h != nil {
			return h
		}
	}
	return sentry.CurrentHub()
}
