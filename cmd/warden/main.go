// Command warden is the gateway guard (ADR 0011, layer 4): a reverse proxy
// in front of LiteLLM that maps each request's key to a room, refuses
// quarantined rooms, and scores rooms' transcripts in the background.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/version"
	"github.com/usr-lex/swarmageddon/internal/warden"
)

func main() {
	var (
		listen      = flag.String("listen", ":4000", "proxy address")
		health      = flag.String("health", ":8080", "health address (/healthz, /readyz)")
		upstream    = flag.String("upstream", "http://litellm."+v1.PlatformNamespace+".svc:4000", "LiteLLM base URL")
		namespace   = flag.String("namespace", v1.PlatformNamespace, "namespace of ConfigMap warden-keys")
		msgURL      = flag.String("messaging-url", "https://messaging."+v1.PlatformNamespace+".svc:8443", "messaging URL; empty: log verdicts only")
		cert        = flag.String("client-cert", "/etc/warden/tls/tls.crt", "client certificate (CN=warden)")
		key         = flag.String("client-key", "/etc/warden/tls/tls.key", "client key")
		ca          = flag.String("ca", "/etc/warden/tls/ca.crt", "platform CA")
		reviewKey   = flag.String("review-key-file", "/etc/warden/review/key", "gateway key for model reviews; missing: rules only")
		reviewEvery = flag.Duration("review-every", warden.DefaultReviewEvery, "least time between two model reviews of a room")
		maxBody     = flag.Int64("max-body", warden.DefaultMaxBody, "largest request body scored, in bytes")
		showVersion = flag.Bool("version", false, "print the version")
	)
	flag.Parse()
	if *showVersion {
		println(version.Version)
		return
	}
	log := slog.New(redact.NewHandler(slog.NewJSONHandler(os.Stderr, nil), nil))
	if err := run(log, *listen, *health, *upstream, *namespace, *msgURL, *cert, *key, *ca, *reviewKey, *reviewEvery, *maxBody); err != nil {
		log.Error("warden", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen, health, upstream, ns, msgURL, cert, key, ca, reviewKey string, reviewEvery time.Duration, maxBody int64) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	up, err := url.Parse(upstream)
	if err != nil {
		return err
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		return err
	}
	kc, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	p := &warden.Proxy{Upstream: up, MaxBody: maxBody, Log: log}
	s := &warden.Scorer{Quarantiner: &warden.RoomQuarantiner{Client: kc}, Proxy: p, Log: log, ReviewEvery: reviewEvery,
		Reviewer: &warden.LLMReviewer{BaseURL: upstream, KeyFile: reviewKey}}
	if msgURL != "" {
		r, err := warden.NewMessagingReporter(msgURL, cert, key, ca)
		if err != nil {
			log.Warn("verdicts will only be logged", "err", err)
		} else {
			s.Reporter = r
		}
	}
	p.Scorer = s
	s.Start(ctx)
	synced := warden.WatchTable(ctx, cs, ns, p, log)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !synced() {
			http.Error(w, "key table not loaded", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	hs := &http.Server{Addr: health, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	ps := &http.Server{Addr: listen, Handler: p, ReadHeaderTimeout: 30 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- hs.ListenAndServe() }()
	go func() { errs <- ps.ListenAndServe() }()
	log.Info("warden listening", "addr", listen, "upstream", upstream)
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	return ps.Shutdown(sctx)
}
