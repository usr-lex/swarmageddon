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
)

func main() {
	dir := flag.String("dir", pluginapi.DevicePluginPath, "kubelet's device plugin directory")
	units := flag.Int("units", devices.DefaultUnits, "units advertised per resource")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plugins := []*devices.Plugin{
		{Resource: devices.ResourceFuse, HostPath: "/dev/fuse", Units: *units, Dir: *dir, Log: log},
		{Resource: devices.ResourceTun, HostPath: "/dev/net/tun", Units: *units, Dir: *dir, Log: log},
	}
	var wg sync.WaitGroup
	for _, p := range plugins {
		wg.Go(func() { _ = p.Run(ctx) })
	}
	wg.Wait()
}
