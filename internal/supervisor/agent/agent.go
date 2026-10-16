// Package agent is the small interface between the room supervisor and a CLI
// driver (Claude Code or ore).
package agent

import (
	"context"
	"fmt"
	"strings"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// Agent is a CLI driver.
type Agent interface {
	// Run starts the CLI and keeps it running (restarting it on exit) until
	// ctx ends. The agent gets a first turn after every start.
	Run(ctx context.Context) error
	// Deliver hands text to the agent: within the current turn when it is
	// busy, as a new turn when it is idle. It returns once the CLI accepted
	// it, and blocks while the CLI is (re)starting.
	Deliver(ctx context.Context, msgID, text string) error
	// Hook handles a CLI hook event reported by `swarm hook`, returning what
	// the hook prints. Drivers without hooks return nil.
	Hook(ctx context.Context, event string, payload []byte, env map[string]string) ([]byte, error)
}

// Events is how a driver reports to the supervisor.
type Events interface {
	Presence(p messagingv1.Presence)
	// Task is the agent's current task (its latest prompt).
	Task(text string)
	// Tokens reports cumulative usage and the current context size.
	Tokens(input, output, context int64)
	// Session reports the CLI's session or thread ID.
	Session(id string)
	// Exited reports that the CLI exited without being asked to.
	Exited(err error)
	// Approve asks people (through messaging) and blocks until answered.
	// Cancelling ctx means the request was answered locally (in the TUI);
	// the supervisor then withdraws it.
	Approve(ctx context.Context, a Approval) (Decision, error)
}

// Approval is a permission request from the CLI.
type Approval struct {
	Tool    string
	Summary string
	Detail  string
	Reason  string
	// SessionScope: the CLI can remember an "allow for this session".
	SessionScope bool
}

// Decision answers an Approval.
type Decision struct {
	Allow      bool
	ForSession bool
	Message    string
}

// Envelope wraps a message for the agent, in the cross-session form Claude
// Code uses, naming the sender and where to reply.
func Envelope(from, fromKind, replyTo, id, body string) string {
	name, _, _ := strings.Cut(from, "@")
	attrs := fmt.Sprintf("from=%q from-name=%q from-mode=\"prompting\"", from, name)
	var hint string
	if fromKind == "human" {
		hint = " (a person)"
	}
	if replyTo == "" {
		replyTo = from
	}
	return fmt.Sprintf("<cross-session-message %s>\nMessage %s from %s%s. Reply with the swarm send_message tool to %s.\n\n%s\n</cross-session-message>",
		attrs, id, from, hint, replyTo, body)
}

// NoticeText formats a platform notice for the agent.
func NoticeText(text string) string {
	return "<swarm-notice>\n" + text + "\n</swarm-notice>"
}
