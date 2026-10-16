package devices

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// fakeKubelet serves the Registration service on dir/kubelet.sock.
type fakeKubelet struct {
	pluginapi.UnimplementedRegistrationServer
	reqs chan *pluginapi.RegisterRequest
}

func (k *fakeKubelet) Register(_ context.Context, r *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	k.reqs <- r
	return &pluginapi.Empty{}, nil
}

func startKubelet(t *testing.T, dir string) *fakeKubelet {
	t.Helper()
	lis, err := net.Listen("unix", filepath.Join(dir, "kubelet.sock"))
	if err != nil {
		t.Fatal(err)
	}
	k := &fakeKubelet{reqs: make(chan *pluginapi.RegisterRequest, 4)}
	srv := grpc.NewServer()
	pluginapi.RegisterRegistrationServer(srv, k)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return k
}

func TestRegisterListAndWatchAllocate(t *testing.T) {
	dir, err := os.MkdirTemp("", "dp") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	kubelet := startKubelet(t, dir)

	dev := filepath.Join(dir, "fuse") // stands in for /dev/fuse
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{Resource: ResourceFuse, HostPath: dev, ContainerPath: "/dev/fuse", Units: 3, Dir: dir, Poll: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	var reg *pluginapi.RegisterRequest
	select {
	case reg = <-kubelet.reqs:
	case <-time.After(5 * time.Second):
		t.Fatal("plugin did not register")
	}
	if reg.ResourceName != ResourceFuse || reg.Version != pluginapi.Version || reg.Endpoint != "swarmageddon-fuse.sock" {
		t.Fatalf("register request: %+v", reg)
	}

	conn, err := grpc.NewClient("unix://"+filepath.Join(dir, reg.Endpoint), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pluginapi.NewDevicePluginClient(conn)

	stream, err := c.ListAndWatch(ctx, &pluginapi.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Devices) != 3 || first.Devices[0].Health != pluginapi.Healthy {
		t.Fatalf("devices: %v", first.Devices)
	}

	// The device disappears: the units turn unhealthy.
	if err := os.Remove(dev); err != nil {
		t.Fatal(err)
	}
	next, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if next.Devices[0].Health != pluginapi.Unhealthy {
		t.Fatalf("after removal: %v", next.Devices)
	}

	resp, err := c.Allocate(ctx, &pluginapi.AllocateRequest{ContainerRequests: []*pluginapi.ContainerAllocateRequest{
		{DevicesIds: []string{"fuse-0", "fuse-1"}}, {DevicesIds: []string{"fuse-2"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ContainerResponses) != 2 {
		t.Fatalf("responses: %v", resp.ContainerResponses)
	}
	for _, cr := range resp.ContainerResponses {
		if len(cr.Devices) != 1 || cr.Devices[0].HostPath != dev || cr.Devices[0].ContainerPath != "/dev/fuse" || cr.Devices[0].Permissions != "rw" {
			t.Fatalf("container response: %v", cr.Devices)
		}
	}

	// Kubelet restarts: it wipes the directory; the plugin registers again.
	if err := os.Remove(filepath.Join(dir, reg.Endpoint)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-kubelet.reqs:
	case <-time.After(5 * time.Second):
		t.Fatal("plugin did not register again")
	}
}
