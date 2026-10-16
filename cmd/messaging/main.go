// Command messaging is the messaging service: Connect API over mTLS in front
// of an embedded, clustered NATS JetStream (docs/design.md §7, ADR 0002).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/messaging/directory"
	"github.com/usr-lex/swarmageddon/internal/messaging/server"
	"github.com/usr-lex/swarmageddon/internal/messaging/store"
	"github.com/usr-lex/swarmageddon/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("messaging", "err", err)
		os.Exit(1)
	}
}

func run() error {
	host, _ := os.Hostname()
	var (
		listen      = flag.String("listen", ":8443", "Connect API address (HTTPS, mTLS)")
		health      = flag.String("health", ":8080", "health address (/healthz, /readyz)")
		tlsCert     = flag.String("tls-cert", "/etc/messaging/tls/tls.crt", "serving certificate")
		tlsKey      = flag.String("tls-key", "/etc/messaging/tls/tls.key", "serving key")
		caFile      = flag.String("ca", "/etc/messaging/tls/ca.crt", "CA bundle that client certificates must chain to")
		dataDir     = flag.String("data", "/data", "JetStream storage directory")
		routes      = flag.String("nats-routes", "", "comma-separated NATS route URLs of every replica (host:6222 or nats://host:6222); empty runs a single node")
		serverName  = flag.String("server-name", host, "NATS server name, unique and stable per replica (the pod name)")
		clusterSize = flag.Int("cluster-size", 0, "number of replicas; stream replicas = min(3, size). 0: the number of routes, or 1")
		namespace   = flag.String("namespace", "swarmageddon-platform", "namespace the service runs in")
		devInsecure = flag.Bool("dev-insecure", false, "DEVELOPMENT ONLY: plain HTTP/2 (h2c), caller identity from the x-swarm-dev-identity header")
		dirFile     = flag.String("directory-file", "", "DEVELOPMENT ONLY: read workspaces, rooms and role templates from this YAML file instead of Kubernetes")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return nil
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	log.Info("starting", "version", version.Version, "server", *serverName, "namespace", *namespace)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Directory.
	var dir directory.Directory
	if *dirFile != "" {
		d, err := directory.LoadFile(*dirFile)
		if err != nil {
			return err
		}
		dir = d
	} else {
		cfg, err := config.GetConfig()
		if err != nil {
			return fmt.Errorf("kubernetes config: %w", err)
		}
		k, err := directory.NewKube(cfg, log)
		if err != nil {
			return err
		}
		go func() {
			if err := k.Start(ctx); err != nil {
				log.Error("directory", "err", err)
				stop()
			}
		}()
		dir = k
	}

	// Health first, so the pod is live while the cluster forms.
	var ready atomic.Bool
	hmux := http.NewServeMux()
	hmux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	hmux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() || !dir.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	hsrv := &http.Server{Addr: *health, Handler: hmux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := hsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "err", err)
			stop()
		}
	}()
	defer func() { _ = hsrv.Close() }()

	// NATS and the store.
	node, err := store.StartNode(store.NodeConfig{
		ServerName:  *serverName,
		DataDir:     *dataDir,
		ClientPort:  4222,
		ClusterHost: "0.0.0.0",
		ClusterPort: 6222,
		Routes:      routeURLs(*routes),
		ClusterSize: *clusterSize,
		Log:         log,
	})
	if err != nil {
		return err
	}
	defer node.Close()
	st, err := store.Open(ctx, node, log, store.Options{})
	if err != nil {
		return err
	}
	srv := server.New(dir, st, log)
	go srv.Run(ctx)

	// API.
	mux := http.NewServeMux()
	mux.Handle(messagingv1connect.NewMessagingServiceHandler(srv))
	reqCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	api := &http.Server{Addr: *listen, Handler: server.Identify(mux, *devInsecure), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return reqCtx }}
	if *devInsecure {
		log.Warn("--dev-insecure: no TLS, identities taken from a header; never use this in a cluster")
		api.Protocols = new(http.Protocols)
		api.Protocols.SetHTTP1(true)
		api.Protocols.SetUnencryptedHTTP2(true)
	} else {
		tc, err := server.TLSConfig(*tlsCert, *tlsKey, *caFile)
		if err != nil {
			return err
		}
		api.TLSConfig = tc
	}
	errc := make(chan error, 1)
	go func() {
		var err error
		if *devInsecure {
			err = api.ListenAndServe()
		} else {
			err = api.ListenAndServeTLS("", "")
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	ready.Store(true)
	log.Info("serving", "listen", *listen, "health", *health)

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	ready.Store(false)
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Streams (Subscribe, Watch*) never finish on their own; end them so
	// clients reconnect to another replica.
	cancelRequests()
	_ = api.Shutdown(sctx)
	return nil
}

// routeURLs splits the --nats-routes flag, adding nats:// where missing.
func routeURLs(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.Contains(r, "://") {
			r = "nats://" + r
		}
		out = append(out, r)
	}
	return out
}
