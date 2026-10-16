package swarm

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	roomv1 "github.com/usr-lex/swarmageddon/gen/swarm/room/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/room/v1/roomv1connect"
	"github.com/usr-lex/swarmageddon/internal/version"
)

// SendInput is send_message's input.
type SendInput struct {
	To      string `json:"to" jsonschema:"recipient: a name in your workspace, or name@workspace"`
	Body    string `json:"body" jsonschema:"the message text"`
	ReplyTo string `json:"reply_to,omitempty" jsonschema:"optional address replies should go to"`
}

// StatusInput is set_status's input.
type StatusInput struct {
	CurrentTask string `json:"current_task" jsonschema:"one line saying what you are working on"`
}

// Empty is the input of tools without arguments.
type Empty struct{}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func failed(err error) *mcp.CallToolResult {
	msg := err.Error()
	if ce := new(connect.Error); asConnect(err, &ce) {
		msg = ce.Message()
	}
	r := text(msg)
	r.IsError = true
	return r
}

func asConnect(err error, target **connect.Error) bool {
	ce, ok := err.(*connect.Error)
	if ok {
		*target = ce
	}
	return ok
}

func jsonText(v any) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return text(string(b))
}

// NewMCPServer returns the swarm MCP server with its four tools.
func NewMCPServer(c roomv1connect.SupervisorServiceClient) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "swarm", Version: version.Version}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "send_message",
		Description: "Send a message to another agent or a person on your team. Delivery is checked against the workspace's messaging policy; a refusal explains what is allowed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, any, error) {
		res, err := c.SendMessage(ctx, connect.NewRequest(&roomv1.SendMessageRequest{To: in.To, Body: in.Body, ReplyTo: in.ReplyTo}))
		if err != nil {
			return failed(err), nil, nil
		}
		return text(fmt.Sprintf("Sent (id %s).", res.Msg.GetId())), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the agents and people you can message, with their role, group and presence (working, waitingOnHuman, idle, offline).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
		res, err := c.ListAgents(ctx, connect.NewRequest(&roomv1.ListAgentsRequest{}))
		if err != nil {
			return failed(err), nil, nil
		}
		return jsonText(res.Msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "whoami",
		Description: "Your own address, workspace, group, role and forge account.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
		res, err := c.WhoAmI(ctx, connect.NewRequest(&roomv1.WhoAmIRequest{}))
		if err != nil {
			return failed(err), nil, nil
		}
		return jsonText(res.Msg), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "set_status",
		Description: "Set the one-line current task people see for you in the control center.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in StatusInput) (*mcp.CallToolResult, any, error) {
		if _, err := c.SetStatus(ctx, connect.NewRequest(&roomv1.SetStatusRequest{CurrentTask: in.CurrentTask})); err != nil {
			return failed(err), nil, nil
		}
		return text("Status set."), nil, nil
	})
	return s
}

// ServeMCP runs the MCP server on stdio until the client disconnects.
func ServeMCP(ctx context.Context, c roomv1connect.SupervisorServiceClient) error {
	return NewMCPServer(c).Run(ctx, &mcp.StdioTransport{})
}
