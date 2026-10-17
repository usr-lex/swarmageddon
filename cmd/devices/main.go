// Command devices is swarmageddon's device plugin (ADR 0001): it runs as a
// DaemonSet and advertises swarmageddon.dev/fuse and swarmageddon.dev/tun,
// so pods in the userns tier get /dev/fuse and /dev/net/tun without being
// privileged.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/usr-lex/swarmageddon/internal/devices"
	"github.com/usr-lex/swarmageddon/internal/telemetry"
)

func main() {
	dir := flag.String("dir", pluginapi.DevicePluginPath, "kubelet's device plugin directory")
	hostRoot := flag.String("host-root", "", "where the host's /dev is mounted, as <host-root>/dev (empty: this container's /dev is the host's)")
	units := flag.Int("units", devices.DefaultUnits, "units advertised per resource")
	flag.Parse()
	flush, terr := telemetry.Init("devices")
	defer flush()
	log := slog.New(telemetry.LogHandler(slog.NewJSONHandler(os.Stderr, nil)))
	if terr != nil {
		log.Warn("error reporting is off", "err", terr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plugins := []*devices.Plugin{
		{Resource: devices.ResourceFuse, HostPath: "/dev/fuse", HostRoot: *hostRoot, Units: *units, Dir: *dir, Log: log},
		{Resource: devices.ResourceTun, HostPath: "/dev/net/tun", HostRoot: *hostRoot, Units: *units, Dir: *dir, Log: log},
	}
	var wg sync.WaitGroup
	for _, p := range plugins {
		wg.Go(func() { _ = p.Run(ctx) })
	}
	wg.Wait()
}
