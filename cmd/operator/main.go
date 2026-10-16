// Command operator runs swarmageddon's controllers (docs/design.md §5):
// Platform, Workspace, Room and RunnerPool, leader-elected.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
	"github.com/usr-lex/swarmageddon/internal/operator/platform"
	"github.com/usr-lex/swarmageddon/internal/operator/preflight"
	"github.com/usr-lex/swarmageddon/internal/operator/room"
	"github.com/usr-lex/swarmageddon/internal/operator/runnerpool"
	"github.com/usr-lex/swarmageddon/internal/operator/workspace"
	"github.com/usr-lex/swarmageddon/internal/pki"
	"github.com/usr-lex/swarmageddon/internal/version"
)

func main() {
	var (
		metricsAddr = flag.String("metrics-bind-address", ":8080", "address the metrics endpoint binds to")
		probeAddr   = flag.String("health-probe-bind-address", ":8081", "address the health probes bind to")
		leaderElect = flag.Bool("leader-elect", false, "elect a leader, so only one replica is active")
		imagesFrom  = flag.String("images-from", "", "YAML or JSON file overriding image references (keys as in internal/operator/images)")
		roomImage   = flag.String("room-image", "", "room image, overriding the default and --images-from")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	if len(os.Args) > 1 && os.Args[1] == "welcome" {
		if err := welcome(15 * time.Minute); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	if err := run(*metricsAddr, *probeAddr, *leaderElect, *imagesFrom, *roomImage); err != nil {
		log.Error(err, "operator failed")
		os.Exit(1)
	}
}

func run(metricsAddr, probeAddr string, leaderElect bool, imagesFrom, roomImage string) error {
	log := ctrl.Log.WithName("setup")
	imgs, err := images.Load(imagesFrom, version.Version)
	if err != nil {
		return fmt.Errorf("images: %w", err)
	}
	if roomImage != "" {
		imgs.Room = roomImage
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	scheme, err := newScheme()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "swarmageddon-operator.swarmageddon.dev",
		LeaderElectionNamespace: v1alpha1.SystemNamespace,
	})
	if err != nil {
		return err
	}

	// The CA must exist before controllers run, since deps.PKI has no context.
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	store := &pki.Store{Client: direct, Namespace: v1alpha1.SystemNamespace}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	ca, err := store.CA(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("platform CA: %w", err)
	}
	log.Info("platform CA loaded", "fingerprint", ca.Fingerprint())

	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	c := mgr.GetClient()
	d := deps.Deps{
		PKI:          ca,
		Images:       deps.ImageRefs(imgs),
		Forge:        ForgeFactory(c),
		Gateway:      GatewayFactory(c),
		PlatformTier: PlatformTier(c),
		Clock:        clock.RealClock{},
		CAStore:      store,
		Pinned:       imgs,
		Version:      version.Version,
	}
	pf := &preflight.Runner{Client: c, ServerVersion: dc.ServerVersion, Images: imgs}
	if err := platform.SetupWithManager(mgr, d, pf); err != nil {
		return fmt.Errorf("platform controller: %w", err)
	}
	if err := workspace.SetupWithManager(mgr, d); err != nil {
		return fmt.Errorf("workspace controller: %w", err)
	}
	if err := room.SetupWithManager(mgr, d); err != nil {
		return fmt.Errorf("room controller: %w", err)
	}
	if err := runnerpool.SetupWithManager(mgr, d); err != nil {
		return fmt.Errorf("runnerpool controller: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	log.Info("starting", "version", version.Version, "room", imgs.Room)
	return mgr.Start(ctrl.SetupSignalHandler())
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}
