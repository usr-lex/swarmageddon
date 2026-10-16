// Command supervisor is a room's PID 1 (under tini): it sets up the room and
// runs the agent CLI, its services and the bridge to messaging (design §6.3).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/usr-lex/swarmageddon/internal/supervisor"
	"github.com/usr-lex/swarmageddon/internal/version"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		_, _ = os.Stdout.WriteString(version.Version + "\n")
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Info("supervisor starting", "version", version.Version)
	if err := supervisor.Main(ctx, log); err != nil {
		log.Error("supervisor", "err", err)
		os.Exit(1)
	}
}
