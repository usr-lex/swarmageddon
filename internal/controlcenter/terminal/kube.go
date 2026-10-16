package terminal

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream" //nolint:staticcheck // client-go's spdy dialer still returns these types
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
)

// Kube runs exec and port-forward through the API server.
type Kube struct {
	Config *rest.Config
	Client kubernetes.Interface
	reqID  atomic.Int64
}

type sizeQueue struct {
	ctx context.Context
	ch  <-chan Size
}

func (q sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case <-q.ctx.Done():
		return nil
	case s, ok := <-q.ch:
		if !ok {
			return nil
		}
		return &remotecommand.TerminalSize{Width: s.Cols, Height: s.Rows}
	}
}

// Exec implements Execer with the WebSocket exec protocol (v5), falling
// back to SPDY for API servers that refuse the WebSocket upgrade.
func (k *Kube) Exec(ctx context.Context, ns, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer, resize <-chan Size) error {
	req := k.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdin: true, Stdout: true, TTY: true}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(k.Config, "GET", req.URL().String())
	if err != nil {
		return err
	}
	sp, err := remotecommand.NewSPDYExecutor(k.Config, "POST", req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, sp, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: stdin, Stdout: stdout, Tty: true, TerminalSizeQueue: sizeQueue{ctx: ctx, ch: resize},
	})
}

// Run implements Runner: the same exec protocols, without a TTY or stdin.
func (k *Kube) Run(ctx context.Context, ns, pod, container string, cmd []string, stdout, stderr io.Writer) error {
	req := k.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(k.Config, "GET", req.URL().String())
	if err != nil {
		return err
	}
	sp, err := remotecommand.NewSPDYExecutor(k.Config, "POST", req.URL())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, sp, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
}

// Forward implements Forwarder: one data stream to port in the pod, over
// SPDY (pods/portforward).
func (k *Kube) Forward(ctx context.Context, ns, pod string, port int) (io.ReadWriteCloser, error) {
	req := k.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward")
	tr, upgrader, err := spdy.RoundTripperFor(k.Config)
	if err != nil {
		return nil, err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: tr}, "POST", req.URL())
	conn, proto, err := dialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, err
	}
	if proto != portforward.PortForwardProtocolV1Name {
		_ = conn.Close()
		return nil, fmt.Errorf("port-forward protocol %q not supported", proto)
	}
	id := strconv.FormatInt(k.reqID.Add(1), 10)
	h := http.Header{}
	h.Set(corev1.StreamType, corev1.StreamTypeError)
	h.Set(corev1.PortHeader, strconv.Itoa(port))
	h.Set(corev1.PortForwardRequestIDHeader, id)
	errStream, err := conn.CreateStream(h)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	errStream.Close()
	h.Set(corev1.StreamType, corev1.StreamTypeData)
	data, err := conn.CreateStream(h)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
	return &fwdConn{Stream: data, conn: conn}, nil
}

type fwdConn struct {
	httpstream.Stream
	conn httpstream.Connection
}

func (f *fwdConn) Close() error {
	f.Stream.Close()
	return f.conn.Close()
}
