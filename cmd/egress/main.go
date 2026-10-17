// Command egress is the rooms' egress proxy (docs/design.md §11.2): rooms in
// network mode limited reach the internet only through it, and only the
// hosts on their allowlist.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/egress"
	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/telemetry"
	"github.com/usr-lex/swarmageddon/internal/version"
)

func main() {
	var (
		listen      = flag.String("listen", fmt.Sprintf(":%d", egress.Port), "proxy address")
		health      = flag.String("health", ":8080", "health address (/healthz, /readyz)")
		namespace   = flag.String("namespace", v1.PlatformNamespace, "namespace of ConfigMap "+egress.ConfigMapName)
		logConns    = flag.Bool("log-connections", false, "log every allowed connection's host, not only refusals")
		showVersion = flag.Bool("version", false, "print the version")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	flush, terr := telemetry.Init("egress")
	log := slog.New(redact.NewHandler(telemetry.LogHandler(slog.NewJSONHandler(os.Stderr, nil)), nil))
	if terr != nil {
		log.Warn("error reporting is off", "err", terr)
	}
	if err := run(log, *listen, *health, *namespace, *logConns); err != nil {
		log.Error("egress", "err", err)
		flush()
		os.Exit(1)
	}
	flush()
}

func run(log *slog.Logger, listen, health, ns string, logConns bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	p := &egress.Proxy{Log: log, LogConnections: logConns}
	synced := egress.WatchTable(ctx, cs, ns, p, log)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !synced() {
			http.Error(w, "allowlist not loaded", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	hs := &http.Server{Addr: health, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// Rooms' proxy requests are traced by method only (telemetry.Handler).
	ps := &http.Server{Addr: listen, Handler: telemetry.Handler(p), ReadHeaderTimeout: 30 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- hs.ListenAndServe() }()
	go func() { errs <- ps.ListenAndServe() }()
	log.Info("egress proxy listening", "addr", listen)
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	return ps.Shutdown(sctx)
}
