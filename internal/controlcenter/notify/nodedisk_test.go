package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

func TestNodeDiskAndRestartReason(t *testing.T) {
	ntfy := &hits{}
	ns := ntfy.server(t)
	dir := &fakeDir{settings: v1alpha1.NotificationSettings{Channels: []v1alpha1.NotificationChannel{{Kind: "ntfy", Topic: "acme"}}}}
	tr := NewTracker(&Dispatcher{Dir: dir, NtfyURL: ns.URL}, nil)
	ctx := context.Background()

	low := map[string]string{"acme": "most free on a node: 3 GB, minimum 5 GB", "beta": ""}
	tr.NodeDisk = func(context.Context) (map[string]string, error) { return low, nil }
	tr.Tick(ctx)
	tr.Tick(ctx)
	if ntfy.n() != 1 || !strings.Contains(ntfy.got[0], "acme: node disk almost full") || !strings.Contains(ntfy.got[0], "3 GB") {
		t.Fatalf("node diskLow %q", ntfy.got)
	}
	// Recovered, then low again: a new notification.
	low["acme"] = ""
	tr.Tick(ctx)
	low["acme"] = "most free on a node: 2 GB, minimum 5 GB"
	tr.Tick(ctx)
	if ntfy.n() != 2 {
		t.Fatalf("after recovery: %d", ntfy.n())
	}

	tr.PodRestarts(ctx, "acme", "athena", 2, "")
	tr.PodRestarts(ctx, "acme", "athena", 3, "room: OOMKilled, exit code 137")
	if ntfy.n() != 3 || !strings.Contains(ntfy.got[2], "last ended room: OOMKilled, exit code 137") {
		t.Fatalf("restart %q", ntfy.got)
	}
}
