// Package swarm is the in-room `swarm` tool: the MCP server agents use to
// message each other, the CLI hook helper, and a small CLI for people in a
// shell. Everything goes through the room supervisor's Unix socket.
package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"

	roomv1 "github.com/usr-lex/swarmageddon/gen/swarm/room/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/room/v1/roomv1connect"
)

// DefaultSocket is the supervisor's room API socket.
const DefaultSocket = "/run/swarm/supervisor.sock"

// SocketPath is $SWARM_SOCKET or the default.
func SocketPath() string {
	if s := os.Getenv("SWARM_SOCKET"); s != "" {
		return s
	}
	return DefaultSocket
}

// NewClient returns a room API client over the Unix socket.
func NewClient(socket string) roomv1connect.SupervisorServiceClient {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	return roomv1connect.NewSupervisorServiceClient(hc, "http://supervisor")
}

// HookEnv lists the environment variables passed with a hook event.
var HookEnv = []string{"CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_PROJECT_DIR"}

// Hook reports a hook event read from stdin and writes the supervisor's
// output to stdout. It never fails the CLI: errors are reported on stderr
// and the exit code is 0. Only PermissionRequest waits (for an answer);
// other events give up after a few seconds.
func Hook(ctx context.Context, c roomv1connect.SupervisorServiceClient, event string, stdin io.Reader, stdout, stderr io.Writer) {
	payload, _ := io.ReadAll(stdin)
	env := map[string]string{}
	for _, k := range HookEnv {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	if event != "PermissionRequest" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	res, err := c.Hook(ctx, connect.NewRequest(&roomv1.HookRequest{Event: event, Payload: payload, Env: env}))
	if err != nil {
		fmt.Fprintf(stderr, "swarm hook %s: %v\n", event, err)
		return
	}
	if out := res.Msg.GetOutput(); len(out) > 0 {
		_, _ = stdout.Write(out)
		if out[len(out)-1] != '\n' {
			_, _ = stdout.Write([]byte("\n"))
		}
	}
}

// CLI runs a human subcommand (send, agents, whoami, status).
func CLI(ctx context.Context, c roomv1connect.SupervisorServiceClient, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "send":
		if len(args) < 3 {
			return errors.New("usage: swarm send <to> <message>")
		}
		res, err := c.SendMessage(ctx, connect.NewRequest(&roomv1.SendMessageRequest{To: args[1], Body: strings.Join(args[2:], " ")}))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "sent %s\n", res.Msg.GetId())
	case "agents":
		res, err := c.ListAgents(ctx, connect.NewRequest(&roomv1.ListAgentsRequest{}))
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ADDRESS\tKIND\tROLE\tGROUP\tPRESENCE\tTASK\tNOTE")
		for _, a := range res.Msg.GetAgents() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.GetAddress(), a.GetKind(), a.GetTitle(), a.GetGroup(), a.GetPresence(), a.GetCurrentTask(), a.GetNote())
		}
		return tw.Flush()
	case "whoami":
		res, err := c.WhoAmI(ctx, connect.NewRequest(&roomv1.WhoAmIRequest{}))
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(res.Msg, "", "  ")
		fmt.Fprintln(stdout, string(b))
	case "status":
		if len(args) < 2 {
			return errors.New("usage: swarm status <text>")
		}
		if _, err := c.SetStatus(ctx, connect.NewRequest(&roomv1.SetStatusRequest{CurrentTask: strings.Join(args[1:], " ")})); err != nil {
			return err
		}
	default:
		return errors.New(usage)
	}
	return nil
}

const usage = `usage:
  swarm send <to> <message>   send a message (to: name or name@workspace)
  swarm agents                list who you can message
  swarm whoami                show this room's identity
  swarm status <text>         set the current task shown to people
  swarm mcp                   run the MCP server (stdio)
  swarm hook <Event>          report a Claude Code hook event (stdin: hook JSON)`

// Usage is the command's help text.
func Usage() string { return usage }
