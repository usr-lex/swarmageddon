package swarm

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	roomv1 "github.com/usr-lex/swarmageddon/gen/swarm/room/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/room/v1/roomv1connect"
)

func pjson(m proto.Message) string {
	b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(m)
	return string(b)
}

// errText is a connect error's message without the code prefix.
func errText(err error) string {
	if ce := new(connect.Error); asConnect(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

// Tool inputs (JSON names are what the agent sees).

// PostInput is post's input.
type PostInput struct {
	Channel string `json:"channel" jsonschema:"channel name, with or without #"`
	Body    string `json:"body" jsonschema:"the post; keep it short. @name mentions push to that agent"`
}

// ReadInput is read_messages' input.
type ReadInput struct {
	Scope      string `json:"scope,omitempty" jsonschema:"all (default), direct, mentions, or a channel name"`
	UnreadOnly *bool  `json:"unread_only,omitempty" jsonschema:"only unread items (default true)"`
	Limit      int32  `json:"limit,omitempty" jsonschema:"page size, default 20, at most 50"`
	Cursor     string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page"`
	Full       bool   `json:"full,omitempty" jsonschema:"whole bodies instead of the first 500 characters"`
	MarkRead   *bool  `json:"mark_read,omitempty" jsonschema:"mark returned items read (default true)"`
}

// IDInput is get_message's input.
type IDInput struct {
	ID string `json:"id" jsonschema:"message or post id"`
}

// MarkReadInput is mark_read's input.
type MarkReadInput struct {
	Scope string `json:"scope" jsonschema:"all, direct, mentions, or a channel name"`
	UpTo  string `json:"up_to,omitempty" jsonschema:"item id to mark read up to; empty marks everything in scope"`
}

// DeliveryInput is set_delivery's input.
type DeliveryInput struct {
	Channel string `json:"channel" jsonschema:"channel name"`
	Mode    string `json:"mode" jsonschema:"push, digest or pull"`
}

// AdviseInput is advise's input.
type AdviseInput struct {
	Question       string   `json:"question" jsonschema:"what you want a second opinion on, with the context the advisor needs"`
	Files          []string `json:"files,omitempty" jsonschema:"paths in your work folder the advisor should read"`
	IncludeContext *bool    `json:"include_context,omitempty" jsonschema:"include the recent transcript (default true)"`
}

func channelName(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "#") }

func (in ReadInput) request() *messagingv1.ReadMessagesRequest {
	scope := channelName(in.Scope)
	if scope == "" {
		scope = "all"
	}
	return &messagingv1.ReadMessagesRequest{Scope: scope, UnreadOnly: in.UnreadOnly, Limit: in.Limit, Cursor: in.Cursor, Full: in.Full, MarkRead: in.MarkRead}
}

// addChannelTools registers the channel, reading and advisor tools.
func addChannelTools(s *mcp.Server, c roomv1connect.SupervisorServiceClient) {
	result := func(m proto.Message, err error) (*mcp.CallToolResult, any, error) {
		if err != nil {
			return failed(err), nil, nil
		}
		return text(pjson(m)), nil, nil
	}
	mcp.AddTool(s, &mcp.Tool{Name: "post", Description: "Post to a channel, for things several agents should know. Keep posts short and don't repeat them on several channels; use send_message or an @mention when one agent must act."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in PostInput) (*mcp.CallToolResult, any, error) {
			res, err := c.Post(ctx, connect.NewRequest(&messagingv1.PostRequest{Channel: channelName(in.Channel), Body: in.Body}))
			if err != nil {
				return failed(err), nil, nil
			}
			return result(res.Msg, nil)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "read_messages", Description: "Read a page of direct messages and channel posts, newest last (unread only by default; returned items are marked read)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, any, error) {
			res, err := c.ReadMessages(ctx, connect.NewRequest(in.request()))
			if err != nil {
				return failed(err), nil, nil
			}
			return result(res.Msg, nil)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_message", Description: "Get one message or post in full."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
			res, err := c.GetMessage(ctx, connect.NewRequest(&messagingv1.GetMessageRequest{Id: in.ID}))
			if err != nil {
				return failed(err), nil, nil
			}
			return result(res.Msg, nil)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "mark_read", Description: "Mark messages read without reading them."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in MarkReadInput) (*mcp.CallToolResult, any, error) {
			_, err := c.MarkRead(ctx, connect.NewRequest(&messagingv1.MarkReadRequest{Scope: channelName(in.Scope), UpTo: in.UpTo}))
			if err != nil {
				return failed(err), nil, nil
			}
			return text("Marked read."), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "unread_summary", Description: "Unread counts per channel, for direct messages and for mentions. Check it at natural breaks."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
			res, err := c.UnreadSummary(ctx, connect.NewRequest(&messagingv1.UnreadSummaryRequest{}))
			if err != nil {
				return failed(err), nil, nil
			}
			return result(res.Msg, nil)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_channels", Description: "Channels you can see, with your access, delivery mode and unread count."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ Empty) (*mcp.CallToolResult, any, error) {
			res, err := c.ListChannels(ctx, connect.NewRequest(&messagingv1.ListChannelsRequest{}))
			if err != nil {
				return failed(err), nil, nil
			}
			return result(res.Msg, nil)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "set_delivery", Description: "Choose how a channel reaches you: push (each post), digest (a periodic summary) or pull (only when you read)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in DeliveryInput) (*mcp.CallToolResult, any, error) {
			_, err := c.SetDelivery(ctx, connect.NewRequest(&messagingv1.SetDeliveryRequest{Channel: channelName(in.Channel), Mode: in.Mode}))
			if err != nil {
				return failed(err), nil, nil
			}
			return text("Delivery set."), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "advise", Description: "Ask your advisor, a stronger model, for a second opinion. It sees your role, your question, the recent transcript and the files you name (read-only, inside your work folder). The answer comes back only to you. Calls are limited per hour."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in AdviseInput) (*mcp.CallToolResult, any, error) {
			res, err := c.Advise(ctx, connect.NewRequest(&roomv1.AdviseRequest{Question: in.Question, Files: in.Files, IncludeContext: in.IncludeContext}))
			if err != nil {
				return failed(err), nil, nil
			}
			return text(fmt.Sprintf("%s\n\n(advisor %s; %d calls left this hour)", res.Msg.GetAnswer(), res.Msg.GetModel(), res.Msg.GetRemaining())), nil, nil
		})
}

// channelCLI runs the channel, reading and advise subcommands; ok is false
// for other commands.
func channelCLI(ctx context.Context, c roomv1connect.SupervisorServiceClient, args []string, stdout io.Writer) (ok bool, err error) {
	print := func(m proto.Message, err error) error {
		if err != nil {
			return fmt.Errorf("%s", errText(err))
		}
		fmt.Fprintln(stdout, pjson(m))
		return nil
	}
	switch args[0] {
	case "post":
		if len(args) < 3 {
			return true, fmt.Errorf("usage: swarm post <channel> <message>")
		}
		res, err := c.Post(ctx, connect.NewRequest(&messagingv1.PostRequest{Channel: channelName(args[1]), Body: strings.Join(args[2:], " ")}))
		if err != nil {
			return true, fmt.Errorf("%s", errText(err))
		}
		fmt.Fprintf(stdout, "posted %s\n", res.Msg.GetId())
		return true, nil
	case "read":
		fs := flag.NewFlagSet("read", flag.ContinueOnError)
		all := fs.Bool("all", false, "include read items")
		limit := fs.Int("limit", 20, "page size")
		cursor := fs.String("cursor", "", "next_cursor of the previous page")
		full := fs.Bool("full", false, "whole bodies")
		keep := fs.Bool("keep-unread", false, "don't mark returned items read")
		if err := fs.Parse(args[1:]); err != nil {
			return true, err
		}
		in := ReadInput{Scope: fs.Arg(0), Limit: int32(*limit), Cursor: *cursor, Full: *full}
		if *all {
			f := false
			in.UnreadOnly = &f
		}
		if *keep {
			f := false
			in.MarkRead = &f
		}
		res, err := c.ReadMessages(ctx, connect.NewRequest(in.request()))
		if err != nil {
			return true, print(nil, err)
		}
		return true, print(res.Msg, nil)
	case "get":
		if len(args) != 2 {
			return true, fmt.Errorf("usage: swarm get <id>")
		}
		res, err := c.GetMessage(ctx, connect.NewRequest(&messagingv1.GetMessageRequest{Id: args[1]}))
		if err != nil {
			return true, print(nil, err)
		}
		return true, print(res.Msg, nil)
	case "mark-read":
		if len(args) < 2 {
			return true, fmt.Errorf("usage: swarm mark-read <scope> [up-to-id]")
		}
		up := ""
		if len(args) > 2 {
			up = args[2]
		}
		_, err := c.MarkRead(ctx, connect.NewRequest(&messagingv1.MarkReadRequest{Scope: channelName(args[1]), UpTo: up}))
		if err != nil {
			return true, fmt.Errorf("%s", errText(err))
		}
		return true, nil
	case "unread":
		res, err := c.UnreadSummary(ctx, connect.NewRequest(&messagingv1.UnreadSummaryRequest{}))
		if err != nil {
			return true, print(nil, err)
		}
		for _, u := range res.Msg.GetCounts() {
			fmt.Fprintf(stdout, "%s\t%d\n", u.GetScope(), u.GetUnread())
		}
		return true, nil
	case "channels":
		res, err := c.ListChannels(ctx, connect.NewRequest(&messagingv1.ListChannelsRequest{}))
		if err != nil {
			return true, print(nil, err)
		}
		for _, ch := range res.Msg.GetChannels() {
			fmt.Fprintf(stdout, "#%s\t%s\t%s\t%d unread\t%s\n", ch.GetName(), ch.GetAccess(), ch.GetDelivery(), ch.GetUnread(), ch.GetDescription())
		}
		return true, nil
	case "delivery":
		if len(args) != 3 {
			return true, fmt.Errorf("usage: swarm delivery <channel> push|digest|pull")
		}
		_, err := c.SetDelivery(ctx, connect.NewRequest(&messagingv1.SetDeliveryRequest{Channel: channelName(args[1]), Mode: args[2]}))
		if err != nil {
			return true, fmt.Errorf("%s", errText(err))
		}
		return true, nil
	case "advise":
		fs := flag.NewFlagSet("advise", flag.ContinueOnError)
		var files multiFlag
		fs.Var(&files, "file", "a file in the work folder (repeatable)")
		noCtx := fs.Bool("no-context", false, "don't send the recent transcript")
		if err := fs.Parse(args[1:]); err != nil {
			return true, err
		}
		if fs.NArg() == 0 {
			return true, fmt.Errorf("usage: swarm advise [--file path]... [--no-context] <question>")
		}
		incl := !*noCtx
		res, err := c.Advise(ctx, connect.NewRequest(&roomv1.AdviseRequest{Question: strings.Join(fs.Args(), " "), Files: files, IncludeContext: &incl}))
		if err != nil {
			return true, fmt.Errorf("%s", errText(err))
		}
		fmt.Fprintln(stdout, res.Msg.GetAnswer())
		return true, nil
	}
	return false, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
