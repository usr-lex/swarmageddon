// Command swarm is the in-room tool: `swarm mcp` (MCP server for the agent),
// `swarm hook <Event>` (Claude Code hooks) and a CLI for people.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/usr-lex/swarmageddon/internal/swarm"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	args := os.Args[1:]
	c := swarm.NewClient(swarm.SocketPath())
	switch {
	case len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help":
		fmt.Println(swarm.Usage())
	case args[0] == "mcp":
		if err := swarm.ServeMCP(ctx, c); err != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "swarm mcp:", err)
			os.Exit(1)
		}
	case args[0] == "hook":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: swarm hook <Event>")
			return // never fail a hook
		}
		swarm.Hook(ctx, c, args[1], os.Stdin, os.Stdout, os.Stderr)
	default:
		if err := swarm.CLI(ctx, c, args, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "swarm:", err)
			os.Exit(1)
		}
	}
}
