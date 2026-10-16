// Package store keeps messages, audit records, approvals and presence in an
// embedded NATS JetStream cluster (ADR 0002).
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NodeConfig configures the embedded NATS server.
type NodeConfig struct {
	// ServerName is unique per replica and stable across restarts.
	ServerName string
	// DataDir holds JetStream's files (DataDir/jetstream).
	DataDir string
	// ClientPort is the client port on 127.0.0.1; -1 picks a free one.
	ClientPort int
	// ClusterHost and ClusterPort are where routes listen (e.g. 0.0.0.0:6222).
	ClusterHost string
	ClusterPort int
	// Routes are the cluster's route URLs (nats://host:6222), possibly
	// including this replica's own. Empty runs a single node.
	Routes []string
	// ClusterSize is the number of replicas; stream replicas = min(3, size).
	ClusterSize int
	Log         *slog.Logger
}

// Node is a running embedded NATS server with an in-process client.
type Node struct {
	srv      *server.Server
	nc       *nats.Conn
	js       jetstream.JetStream
	replicas int
	name     string
}

// StartNode starts the server and connects to it in-process. It does not
// wait for the JetStream cluster to elect a leader; see Node.WaitJetStream.
func StartNode(cfg NodeConfig) (*Node, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	opts := &server.Options{
		ServerName: cfg.ServerName,
		Host:       "127.0.0.1",
		Port:       cfg.ClientPort,
		JetStream:  true,
		StoreDir:   filepath.Join(cfg.DataDir, "jetstream"),
		NoSigs:     true,
		// Short lame duck so pod shutdown stays within the grace period.
		LameDuckDuration:    5 * time.Second,
		LameDuckGracePeriod: time.Second,
	}
	if len(cfg.Routes) > 0 {
		opts.Cluster = server.ClusterOpts{
			Name: "messaging",
			Host: cfg.ClusterHost,
			Port: cfg.ClusterPort,
		}
		opts.Routes = server.RoutesFromStr(strings.Join(cfg.Routes, ","))
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("nats server: %w", err)
	}
	srv.SetLoggerV2(&natsLogger{cfg.Log.With("component", "nats")}, false, false, false)
	go srv.Start()
	if !srv.ReadyForConnections(30 * time.Second) {
		srv.Shutdown()
		return nil, errors.New("nats server did not start")
	}
	nc, err := nats.Connect("", nats.InProcessServer(srv), nats.Name(cfg.ServerName), nats.MaxReconnects(-1))
	if err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("connect in-process: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		return nil, err
	}
	size := cfg.ClusterSize
	if size < 1 {
		size = max(1, len(cfg.Routes))
	}
	return &Node{srv: srv, nc: nc, js: js, replicas: min(3, size), name: cfg.ServerName}, nil
}

// WaitJetStream blocks until JetStream answers API requests (a meta leader
// exists in a cluster).
func (n *Node) WaitJetStream(ctx context.Context) error {
	for {
		actx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := n.js.AccountInfo(actx)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("jetstream not ready: %w", err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Name is the server name.
func (n *Node) Name() string { return n.name }

// ClusterURL is this node's route URL, for tests.
func (n *Node) ClusterURL() string {
	if a := n.srv.ClusterAddr(); a != nil {
		return fmt.Sprintf("nats://127.0.0.1:%d", a.Port)
	}
	return ""
}

// Close drains the client and shuts the server down.
func (n *Node) Close() {
	n.nc.Close()
	n.srv.Shutdown()
	n.srv.WaitForShutdown()
}

type natsLogger struct{ l *slog.Logger }

func (l *natsLogger) Noticef(format string, v ...any) { l.l.Info(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Warnf(format string, v ...any)   { l.l.Warn(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Fatalf(format string, v ...any)  { l.l.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Errorf(format string, v ...any)  { l.l.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Debugf(format string, v ...any)  { l.l.Debug(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Tracef(format string, v ...any)  {}
