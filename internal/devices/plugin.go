// Package devices is a minimal Kubernetes device plugin (deviceplugin
// v1beta1) that shares host device nodes, such as /dev/fuse and
// /dev/net/tun, with pods in the userns isolation tier (ADR 0001). Each
// resource advertises many identical units; allocating any number of them
// mounts the one host device node into the container.
package devices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Resource names the operator requests in room and runner pods.
const (
	ResourceFuse = "swarmageddon.dev/fuse"
	ResourceTun  = "swarmageddon.dev/tun"
)

// DefaultUnits is how many units of each resource a node advertises: enough
// for every pod a node can hold (kubelet's default maxPods is 110).
const DefaultUnits = 250

// Plugin serves one resource backed by one host device node.
type Plugin struct {
	pluginapi.UnimplementedDevicePluginServer

	// Resource is the extended resource name, e.g. swarmageddon.dev/fuse.
	Resource string
	// HostPath is the device node on the host; ContainerPath where it
	// appears in the container (defaults to HostPath).
	HostPath, ContainerPath string
	// Units advertised.
	Units int
	// Dir is kubelet's device plugin directory (pluginapi.DevicePluginPath).
	Dir string
	// Log; nil uses slog.Default.
	Log *slog.Logger
	// Poll is how often health and the plugin's socket are checked; 0 is 10s.
	Poll time.Duration

	mu      sync.Mutex
	healthy bool
	changed chan struct{} // closed and replaced when health changes
}

// socketName is the plugin's socket file in Dir.
func (p *Plugin) socketName() string {
	return "swarmageddon-" + strings.ReplaceAll(p.Resource[strings.LastIndex(p.Resource, "/")+1:], ".", "-") + ".sock"
}

func (p *Plugin) log() *slog.Logger {
	if p.Log != nil {
		return p.Log.With("resource", p.Resource)
	}
	return slog.Default().With("resource", p.Resource)
}

func (p *Plugin) poll() time.Duration {
	if p.Poll > 0 {
		return p.Poll
	}
	return 10 * time.Second
}

// Run serves the plugin until ctx ends. It registers with kubelet, and
// serves and registers again whenever kubelet restarts (which removes the
// plugin's socket).
func (p *Plugin) Run(ctx context.Context) error {
	p.checkHealth()
	for {
		err := p.serveOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			p.log().Error("device plugin", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(p.poll()):
		}
	}
}

// serveOnce starts the gRPC server, registers it and returns when its
// socket disappears or ctx ends.
func (p *Plugin) serveOnce(ctx context.Context) error {
	sock := filepath.Join(p.Dir, p.socketName())
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(srv, p)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	defer func() {
		srv.Stop()
		_ = os.Remove(sock)
	}()
	if err := p.register(ctx); err != nil {
		return fmt.Errorf("register with kubelet: %w", err)
	}
	p.log().Info("registered with kubelet", "socket", sock, "units", p.Units)
	t := time.NewTicker(p.poll())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			return err
		case <-t.C:
			p.checkHealth()
			if _, err := os.Stat(sock); err != nil {
				p.log().Info("socket removed (kubelet restarted?); registering again")
				return nil
			}
		}
	}
}

func (p *Plugin) register(ctx context.Context) error {
	conn, err := grpc.NewClient("unix://"+filepath.Join(p.Dir, filepath.Base(pluginapi.KubeletSocket)),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = pluginapi.NewRegistrationClient(conn).Register(rctx, &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     p.socketName(),
		ResourceName: p.Resource,
		Options:      &pluginapi.DevicePluginOptions{},
	})
	return err
}

// checkHealth records whether the host device exists, and wakes
// ListAndWatch streams when that changes.
func (p *Plugin) checkHealth() {
	_, err := os.Stat(p.HostPath)
	ok := err == nil
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.changed == nil {
		p.changed = make(chan struct{})
	}
	if ok != p.healthy {
		p.healthy = ok
		close(p.changed)
		p.changed = make(chan struct{})
	}
}

func (p *Plugin) devices() ([]*pluginapi.Device, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.changed == nil {
		p.changed = make(chan struct{})
	}
	health := pluginapi.Unhealthy
	if p.healthy {
		health = pluginapi.Healthy
	}
	out := make([]*pluginapi.Device, p.Units)
	for i := range out {
		out[i] = &pluginapi.Device{ID: fmt.Sprintf("%s-%d", filepath.Base(p.HostPath), i), Health: health}
	}
	return out, p.changed
}

// GetDevicePluginOptions: no pre-start hook, no preferred allocation.
func (p *Plugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch sends the units, and again whenever their health changes.
func (p *Plugin) ListAndWatch(_ *pluginapi.Empty, s grpc.ServerStreamingServer[pluginapi.ListAndWatchResponse]) error {
	for {
		devs, changed := p.devices()
		if err := s.Send(&pluginapi.ListAndWatchResponse{Devices: devs}); err != nil {
			return err
		}
		select {
		case <-s.Context().Done():
			return nil
		case <-changed:
		}
	}
}

// Allocate mounts the host device node into each container, whatever
// number of units it asked for.
func (p *Plugin) Allocate(_ context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	cpath := p.ContainerPath
	if cpath == "" {
		cpath = p.HostPath
	}
	resp := &pluginapi.AllocateResponse{}
	for _, cr := range req.ContainerRequests {
		if len(cr.DevicesIds) == 0 {
			return nil, errors.New("no devices requested")
		}
		resp.ContainerResponses = append(resp.ContainerResponses, &pluginapi.ContainerAllocateResponse{
			Devices: []*pluginapi.DeviceSpec{{HostPath: p.HostPath, ContainerPath: cpath, Permissions: "rw"}},
		})
	}
	return resp, nil
}
