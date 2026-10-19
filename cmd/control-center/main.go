// Command control-center is swarmageddon's web control center (docs/design.md
// §8): sign-in, wizards, fleet, terminals, approvals, notifications and the
// swarm observatory, served as a Connect API plus the embedded UI.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/usr-lex/swarmageddon/internal/controlcenter/api"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/approvals"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/audit"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/msgclient"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/observatory"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/spend"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/terminal"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/redact"
	"github.com/usr-lex/swarmageddon/internal/telemetry"
	"github.com/usr-lex/swarmageddon/internal/version"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

func main() {
	var (
		listen     = flag.String("listen", ":8443", "address of the API and UI")
		health     = flag.String("health", ":8080", "address of /healthz and /readyz")
		tlsCert    = flag.String("tls-cert", "/etc/swarm/tls/tls.crt", "serving certificate")
		tlsKey     = flag.String("tls-key", "/etc/swarm/tls/tls.key", "serving key")
		msgURL     = flag.String("messaging-url", "https://messaging."+v1alpha1.PlatformNamespace+".svc:8443", "messaging service URL; empty disables messaging")
		clientCert = flag.String("client-cert", "/etc/swarm/client/tls.crt", "client certificate for messaging")
		clientKey  = flag.String("client-key", "/etc/swarm/client/tls.key", "client key for messaging")
		caFile     = flag.String("ca", "/etc/swarm/ca/ca.crt", "platform CA bundle")
		dev        = flag.Bool("dev", false, "plain HTTP, no TLS (development only)")
		publicURL  = flag.String("public-url", "", "external base URL (default: Platform status URL or the request host)")
		forgeURL   = flag.String("forge-url", "http://forgejo."+v1alpha1.PlatformNamespace+".svc:3000", "Forgejo's in-cluster URL")
		gatewayURL = flag.String("gateway-url", "http://gateway."+v1alpha1.PlatformNamespace+".svc:4000", "LiteLLM gateway URL")
		ntfyURL    = flag.String("ntfy-url", "http://ntfy."+v1alpha1.PlatformNamespace+".svc", "ntfy URL")
		showVer    = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version.Version)
		return
	}
	flush, terr := telemetry.Init("control-center")
	defer flush()
	// Secret values the control center reads or writes are redacted (§11.4),
	// in Sentry too.
	log := slog.New(redact.NewHandler(telemetry.LogHandler(slog.NewJSONHandler(os.Stderr, nil)), nil))
	slog.SetDefault(log)
	if terr != nil {
		log.Warn("error reporting is off", "err", terr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, options{listen: *listen, health: *health, tlsCert: *tlsCert, tlsKey: *tlsKey, msgURL: *msgURL,
		clientCert: *clientCert, clientKey: *clientKey, ca: *caFile, dev: *dev, publicURL: *publicURL, forgeURL: *forgeURL,
		gatewayURL: *gatewayURL, ntfyURL: *ntfyURL}); err != nil {
		log.Error("control center stopped", "err", err)
		flush()
		os.Exit(1)
	}
}

type options struct {
	listen, health, tlsCert, tlsKey, msgURL, clientCert, clientKey, ca string
	dev                                                                bool
	publicURL, forgeURL, gatewayURL, ntfyURL                           string
}

func run(ctx context.Context, log *slog.Logger, o options) error {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes config: %w", err)
	}
	kc, err := k8s.New(cfg)
	if err != nil {
		return err
	}
	kc.C = k8s.RedactSecrets(kc.C)

	// Session signing key: our own Secret, created on first start.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	var sd map[string][]byte
	for {
		sd, err = kc.CreateSecretIfMissing(ctx, k8s.SecretSession, map[string][]byte{"key": key})
		if err == nil {
			break
		}
		log.Warn("waiting for the session key Secret", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	sessions, err := session.NewManager(sd["key"], !o.dev)
	if err != nil {
		return err
	}

	srv := &api.Server{
		Cfg:      api.Config{PublicURL: o.publicURL, Dev: o.dev, ForgeURL: o.forgeURL, Sentry: telemetry.Browser()},
		K:        kc,
		Sessions: sessions,
		Setup:    auth.NewLimiter(),
		Logins:   auth.NewLimiter(),
		Inbox:    &api.InboxHub{},
		Verdicts: &api.VerdictHub{},
		Exec:     &terminal.Kube{Config: cfg, Client: kc.Kube},
		Log:      log,
	}
	kc2 := &terminal.Kube{Config: cfg, Client: kc.Kube}
	srv.Fwd = kc2
	srv.Audit = audit.New(log, srv.DB)
	sessions.DB = func() session.DB {
		if db := srv.DB(); db != nil {
			return db
		}
		return nil
	}
	sessions.Promote = srv.Promote

	if o.msgURL != "" {
		mc, err := msgclient.New(o.msgURL, o.clientCert, o.clientKey, o.ca)
		if err != nil {
			log.Warn("messaging client not available; messaging features are off", "err", err)
		} else {
			srv.Msg = mc
		}
	}

	// Notifications.
	var vapidOnce sync.Mutex
	var vapid *notify.VAPID
	srv.Notify = &notify.Dispatcher{Dir: srv.NotifyDirectory(), NtfyURL: o.ntfyURL, Log: log, VAPID: func() *notify.VAPID {
		vapidOnce.Lock()
		defer vapidOnce.Unlock()
		if vapid == nil {
			d, err := kc.GetSecret(ctx, k8s.SecretVAPID)
			if err != nil {
				return nil
			}
			sub := "mailto:admin@swarmageddon.invalid"
			if o.publicURL != "" {
				sub = o.publicURL
			}
			vapid = &notify.VAPID{PublicKey: string(d["publicKey"]), PrivateKey: string(d["privateKey"]), Subscriber: sub}
		}
		return vapid
	}}
	// idleWithOpenWork: open issues and pull requests assigned in Forgejo,
	// read with the read-only forge-reader token.
	srv.Tracker = notify.NewTracker(srv.Notify, &notify.ForgeOpenWork{
		Forge: func(ctx context.Context) (*forgejo.Client, error) {
			d, err := kc.GetSecret(ctx, k8s.SecretForgeReader)
			if err != nil {
				return nil, err
			}
			return forgejo.New(o.forgeURL, string(d["token"])), nil
		},
		Org: func(ctx context.Context, ws string) (string, error) {
			w, err := kc.Workspace(ctx, ws)
			if err != nil {
				return "", err
			}
			return w.Spec.Forge.Org, nil
		},
	})
	srv.Tracker.NodeDisk = kc.NodeDiskLow

	// Spend.
	srv.Spend = &spend.Poller{
		Client: &spend.Client{BaseURL: o.gatewayURL, Key: func(ctx context.Context) (string, error) {
			d, err := kc.GetSecret(ctx, k8s.SecretGateway)
			if err != nil {
				return "", err
			}
			return string(d["masterKey"]), nil
		}},
		Budget: func(ctx context.Context, ws string) float64 {
			if w, err := kc.Workspace(ctx, ws); err == nil {
				return float64(w.Spec.Models.Budgets.PerRoomMonthlyUSD)
			}
			return 0
		},
		Raise: srv.Notify.Raise,
		Clear: srv.Notify.Clear,
	}

	// Observatory.
	srv.Rec = &observatory.Recorder{DB: srv.DB, Log: log}
	srv.Presence = &observatory.Presence{R: srv.Rec}

	// Approvals and fleet.
	var msgForApprovals approvals.Messaging = noMessaging{}
	if srv.Msg != nil {
		msgForApprovals = srv.Msg
	}
	srv.Approvals = approvals.NewManager(msgForApprovals, srv.ApprovalLookup())
	srv.Approvals.Audit = srv.Audit.Record
	srv.Fleet = api.NewFleet(kc, srv.Approvals)
	srv.Fleet.Log = log
	srv.Fleet.Spend = func() map[string]float64 {
		out := map[string]float64{}
		for _, k := range srv.Spend.Latest().Rooms {
			out[k.Workspace+"/"+k.Room] = k.Spend
		}
		return out
	}
	srv.Fleet.OnPodRestarts = srv.Tracker.PodRestarts
	srv.Fleet.Badges = func(ctx context.Context) map[string]api.VerdictBadge {
		db := srv.DB()
		if db == nil {
			return nil
		}
		b, _ := db.VerdictBadges(ctx, time.Now().Add(-24*time.Hour))
		return b
	}
	srv.Fleet.OnPresence = func(ctx context.Context, ws, room string, p messagingv1.Presence, at time.Time, st *messagingv1.RoomStatusReport) {
		srv.Tracker.Presence(ctx, ws, room, p, st)
		srv.Presence.Event(ctx, ws, room, p, at, st)
	}
	srv.Approvals.Persist = func(ctx context.Context, a store.Approval) {
		if db := srv.DB(); db != nil {
			_ = db.UpsertApproval(ctx, a)
		}
		srv.Fleet.ApprovalChanged(a.Workspace, a.Room)
	}
	srv.Approvals.Notify = func(ctx context.Context, a approvals.Approval) {
		srv.Notify.Raise(ctx, notify.Event{Kind: v1alpha1.EventApprovalWaiting, Workspace: a.Workspace, Room: a.Room, Fingerprint: a.ID,
			Title: a.Room + " needs an approval", Body: a.Tool + ": " + a.Summary, URL: "/approvals/" + a.ID})
	}

	// Background work.
	go connectDB(ctx, log, kc, srv)
	go srv.Fleet.Run(ctx)
	go srv.Approvals.ExpireLoop(ctx)
	go srv.Tracker.Loop(ctx)
	go srv.Spend.Run(ctx)
	go srv.Audit.RetentionLoop(ctx, srv.AuditRetention)
	go observatory.RetentionLoop(ctx, srv.DB, srv.Retentions)
	if srv.Msg != nil {
		go msgclient.Loop(ctx, "approvals", srv.Approvals.Watch)
		go msgclient.Loop(ctx, "presence", func(ctx context.Context) error { return srv.Fleet.WatchPresence(ctx, srv.Msg) })
		follower := &observatory.AuditFollower{Msg: srv.Msg, R: srv.Rec, Workspaces: srv.WorkspaceNames, IsRoom: srv.IsRoom, OnVerdict: srv.OnVerdict}
		go follower.Run(ctx)
		reports := &observatory.Reports{Msg: srv.Msg, DB: srv.DB}
		go srv.SubscriptionLoop(ctx, reports.Follow)
	}

	// HTTP.
	apiSrv := &http.Server{Addr: o.listen, Handler: telemetry.Handler(srv.Handler()), ReadHeaderTimeout: 10 * time.Second}
	hs := &http.Server{Addr: o.health, Handler: srv.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- hs.ListenAndServe() }()
	go func() {
		if o.dev {
			log.Warn("serving plain HTTP (--dev)", "listen", o.listen)
			errc <- apiSrv.ListenAndServe()
			return
		}
		cr := &certReloader{cert: o.tlsCert, key: o.tlsKey}
		if err := waitForCert(ctx, log, cr, 2*time.Second); err != nil {
			errc <- fmt.Errorf("serving certificate: %w", err)
			return
		}
		apiSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: cr.get}
		log.Info("serving", "listen", o.listen, "version", version.Version)
		errc <- apiSrv.ListenAndServeTLS("", "")
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(shut)
	_ = hs.Shutdown(shut)
	return nil
}

// connectDB waits for Secret control-center-db, connects and migrates, then
// copies the bootstrap-phase audit events in and cleans sessions up hourly.
func connectDB(ctx context.Context, log *slog.Logger, kc *k8s.Client, srv *api.Server) {
	for ctx.Err() == nil {
		d, err := kc.GetSecret(ctx, k8s.SecretDB)
		if err == nil && len(d["url"]) > 0 {
			db, err := store.Open(ctx, string(d["url"]))
			if err == nil {
				srv.SetDB(db)
				log.Info("connected to PostgreSQL; sessions and audit now live there")
				if err := srv.Audit.Flush(ctx); err != nil {
					log.Warn("copying bootstrap audit events", "err", err)
				}
				t := time.NewTicker(time.Hour)
				defer t.Stop()
				for {
					if n, err := db.DeleteExpiredSessions(ctx, time.Now(), session.DefaultIdle); err == nil && n > 0 {
						log.Info("expired sessions removed", "count", n)
					}
					select {
					case <-ctx.Done():
						db.Close()
						return
					case <-t.C:
					}
				}
			}
			log.Warn("PostgreSQL not reachable yet", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// waitForCert waits until the serving certificate loads. On a fresh install
// the operator creates Secret control-center-tls after this pod starts (the
// volume is optional), and the kubelet projects it in place once it exists.
func waitForCert(ctx context.Context, log *slog.Logger, cr *certReloader, every time.Duration) error {
	logged := false
	for {
		_, err := cr.get(nil)
		if err == nil {
			return nil
		}
		if !logged {
			log.Info("waiting for the serving certificate (Secret control-center-tls)", "err", err)
			logged = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last: %v)", ctx.Err(), err)
		case <-time.After(every):
		}
	}
}

// certReloader re-reads the serving certificate when its files change
// (Secret updates are projected into the pod in place).
type certReloader struct {
	cert, key string
	mu        sync.Mutex
	cached    *tls.Certificate
	modTime   time.Time
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := os.Stat(c.cert)
	if err != nil {
		if c.cached != nil {
			return c.cached, nil
		}
		return nil, err
	}
	if c.cached == nil || st.ModTime().After(c.modTime) {
		kp, err := tls.LoadX509KeyPair(c.cert, c.key)
		if err != nil {
			if c.cached != nil {
				return c.cached, nil
			}
			return nil, err
		}
		c.cached, c.modTime = &kp, st.ModTime()
	}
	return c.cached, nil
}

// noMessaging answers approvals calls when messaging isn't configured.
type noMessaging struct{}

func (noMessaging) WatchApprovals(context.Context, *connect.Request[messagingv1.WatchApprovalsRequest]) (*connect.ServerStreamForClient[messagingv1.ApprovalEvent], error) {
	return nil, errors.New("messaging is not configured")
}

func (noMessaging) AnswerApproval(context.Context, *connect.Request[messagingv1.AnswerApprovalRequest]) (*connect.Response[messagingv1.AnswerApprovalResponse], error) {
	return nil, errors.New("messaging is not configured")
}
