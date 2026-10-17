package ore

import (
	"context"
	"slices"
	"testing"
)

func TestRestartSetsEnv(t *testing.T) {
	d := &Driver{CodexHome: "/h", Env: []string{GatewayKeyEnv + "=old"}}
	// Not running: only the next start sees the new key.
	d.Restart(map[string]string{GatewayKeyEnv: "new"})
	if !slices.Contains(d.env(), GatewayKeyEnv+"=new") || d.restart {
		t.Fatalf("env %q restart %v", d.env(), d.restart)
	}
	// Running: the app-server's context ends and the restart is marked.
	ctx, cancel := context.WithCancel(context.Background())
	d.stop = cancel
	d.Restart(map[string]string{GatewayKeyEnv: "newer"})
	if ctx.Err() == nil || !d.restart || !slices.Contains(d.env(), GatewayKeyEnv+"=newer") {
		t.Fatalf("running restart: ctx %v restart %v env %q", ctx.Err(), d.restart, d.env())
	}
}
