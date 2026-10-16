package supervisor

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
)

// Channels and reading (design §7.8): the room API forwards to messaging,
// which applies policy and keeps read cursors.

// forwardErr makes "not implemented yet" readable for the agent.
func forwardErr(err error) error {
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		return connect.NewError(connect.CodeUnimplemented, errors.New("channels are not available on this platform yet; use send_message"))
	}
	return err
}

func forward[Req, Res any](ctx context.Context, call func(context.Context, *connect.Request[Req]) (*connect.Response[Res], error), r *connect.Request[Req]) (*connect.Response[Res], error) {
	res, err := call(ctx, connect.NewRequest(r.Msg))
	if err != nil {
		return nil, forwardErr(err)
	}
	return connect.NewResponse(res.Msg), nil
}

// Post posts to a channel; the post ID is chosen here (the dedup key).
func (a *API) Post(ctx context.Context, r *connect.Request[messagingv1.PostRequest]) (*connect.Response[messagingv1.PostResponse], error) {
	if r.Msg.GetChannel() == "" || r.Msg.GetBody() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("channel and body are required"))
	}
	if r.Msg.Id == "" {
		r.Msg.Id = ulid.Make().String()
	}
	return forward(ctx, a.Core.Messaging.Post, r)
}

// ReadMessages returns a page of direct messages and posts.
func (a *API) ReadMessages(ctx context.Context, r *connect.Request[messagingv1.ReadMessagesRequest]) (*connect.Response[messagingv1.ReadMessagesResponse], error) {
	return forward(ctx, a.Core.Messaging.ReadMessages, r)
}

// GetMessage returns one item in full.
func (a *API) GetMessage(ctx context.Context, r *connect.Request[messagingv1.GetMessageRequest]) (*connect.Response[messagingv1.GetMessageResponse], error) {
	return forward(ctx, a.Core.Messaging.GetMessage, r)
}

// MarkRead moves a read cursor.
func (a *API) MarkRead(ctx context.Context, r *connect.Request[messagingv1.MarkReadRequest]) (*connect.Response[messagingv1.MarkReadResponse], error) {
	return forward(ctx, a.Core.Messaging.MarkRead, r)
}

// UnreadSummary returns unread counts.
func (a *API) UnreadSummary(ctx context.Context, r *connect.Request[messagingv1.UnreadSummaryRequest]) (*connect.Response[messagingv1.UnreadSummaryResponse], error) {
	return forward(ctx, a.Core.Messaging.UnreadSummary, r)
}

// ListChannels lists visible channels.
func (a *API) ListChannels(ctx context.Context, r *connect.Request[messagingv1.ListChannelsRequest]) (*connect.Response[messagingv1.ListChannelsResponse], error) {
	return forward(ctx, a.Core.Messaging.ListChannels, r)
}

// SetDelivery changes this agent's delivery mode for a channel.
func (a *API) SetDelivery(ctx context.Context, r *connect.Request[messagingv1.SetDeliveryRequest]) (*connect.Response[messagingv1.SetDeliveryResponse], error) {
	return forward(ctx, a.Core.Messaging.SetDelivery, r)
}
