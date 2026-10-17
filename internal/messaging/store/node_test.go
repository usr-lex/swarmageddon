package store

import (
	"context"
	"testing"
	"time"
)

// One replica whose only route is itself (what the operator renders for
// messaging_replicas 1) must get a JetStream it can use.
func TestSingleReplicaWithOwnRoute(t *testing.T) {
	n, err := StartNode(NodeConfig{
		ServerName:  "messaging-0",
		DataDir:     t.TempDir(),
		ClientPort:  -1,
		ClusterHost: "127.0.0.1",
		ClusterPort: -1,
		Routes:      []string{"nats://127.0.0.1:6222"},
		ClusterSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := n.WaitJetStream(ctx); err != nil {
		t.Fatal(err)
	}
}
