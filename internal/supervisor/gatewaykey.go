package supervisor

import (
	"context"
	"log/slog"
	"time"

	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

// GatewayKeyEvery is how often the supervisor re-reads gateway_key. Kubelet
// updates a mounted Secret within about a minute of a change.
const GatewayKeyEvery = 30 * time.Second

// watchGatewayKey re-reads the gateway key (read) every interval. When the
// operator has rotated it, which revokes the old key, the CLI is restarted
// with the new one in envName (docs/design.md §6.3: rotated credentials are
// reloaded from the Secret mount).
func watchGatewayKey(ctx context.Context, read func() string, current string, a agent.Agent, envName string, every time.Duration, log *slog.Logger) {
	r, ok := a.(agent.Restarter)
	if !ok {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		key := read()
		if key == "" || key == current {
			continue
		}
		current = key
		log.Info("gateway key rotated; restarting the CLI with the new key")
		r.Restart(map[string]string{envName: key})
	}
}
