package supervisor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	roomv1 "github.com/usr-lex/swarmageddon/gen/swarm/room/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/room/v1/roomv1connect"
)

// API serves swarm.room.v1 for `swarm` (MCP server, hooks, CLI).
type API struct {
	roomv1connect.UnimplementedSupervisorServiceHandler
	Core *Core
}

// SendMessage forwards to messaging, which applies policy.
func (a *API) SendMessage(ctx context.Context, r *connect.Request[roomv1.SendMessageRequest]) (*connect.Response[roomv1.SendMessageResponse], error) {
	to, body := strings.TrimSpace(r.Msg.GetTo()), r.Msg.GetBody()
	if to == "" || strings.TrimSpace(body) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("to and body are required"))
	}
	id := ulid.Make().String()
	res, err := a.Core.Messaging.Send(ctx, connect.NewRequest(&messagingv1.SendRequest{Id: id, To: to, Body: body, ReplyTo: r.Msg.GetReplyTo()}))
	if err != nil {
		return nil, err // connect errors (e.g. a policy refusal) pass through
	}
	return connect.NewResponse(&roomv1.SendMessageResponse{Id: res.Msg.GetId()}), nil
}

// PresenceName is the lower-case word for a presence.
func PresenceName(p messagingv1.Presence) string {
	switch p {
	case messagingv1.Presence_PRESENCE_WORKING:
		return "working"
	case messagingv1.Presence_PRESENCE_WAITING_ON_HUMAN:
		return "waitingOnHuman"
	case messagingv1.Presence_PRESENCE_IDLE:
		return "idle"
	case messagingv1.Presence_PRESENCE_OFFLINE:
		return "offline"
	}
	return "unknown"
}

// ListAgents lists who this room may message.
func (a *API) ListAgents(ctx context.Context, _ *connect.Request[roomv1.ListAgentsRequest]) (*connect.Response[roomv1.ListAgentsResponse], error) {
	res, err := a.Core.Messaging.ListPeers(ctx, connect.NewRequest(&messagingv1.ListPeersRequest{}))
	if err != nil {
		return nil, err
	}
	out := &roomv1.ListAgentsResponse{Self: res.Msg.GetSelf()}
	for _, p := range res.Msg.GetPeers() {
		out.Agents = append(out.Agents, &roomv1.Agent{
			Address: p.GetAddress(), Kind: p.GetKind(), Title: p.GetTitle(), Summary: p.GetSummary(),
			Group: p.GetGroup(), Presence: PresenceName(p.GetPresence()), Note: p.GetNote(), CurrentTask: p.GetCurrentTask(),
		})
	}
	return connect.NewResponse(out), nil
}

// WhoAmI describes this room.
func (a *API) WhoAmI(context.Context, *connect.Request[roomv1.WhoAmIRequest]) (*connect.Response[roomv1.WhoAmIResponse], error) {
	r := a.Core.Room
	return connect.NewResponse(&roomv1.WhoAmIResponse{
		Address: r.AddressOrDefault(), Workspace: r.Workspace, Group: r.Group, Role: r.RoleTitle,
		ForgeUser: r.ForgeAccount(), ForgeUrl: r.ForgeURL,
	}), nil
}

// SetStatus sets the current task shown in the control center.
func (a *API) SetStatus(_ context.Context, r *connect.Request[roomv1.SetStatusRequest]) (*connect.Response[roomv1.SetStatusResponse], error) {
	if err := a.Core.SetTask(r.Msg.GetCurrentTask()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&roomv1.SetStatusResponse{}), nil
}

// Hook passes a CLI hook event to the driver. For PermissionRequest it
// blocks until answered; if the hook process goes away (answered in the
// TUI), the request context ends and the approval is withdrawn.
func (a *API) Hook(ctx context.Context, r *connect.Request[roomv1.HookRequest]) (*connect.Response[roomv1.HookResponse], error) {
	out, err := a.Core.Agent.Hook(ctx, r.Msg.GetEvent(), r.Msg.GetPayload(), r.Msg.GetEnv())
	if err != nil {
		if ctx.Err() != nil {
			return nil, connect.NewError(connect.CodeCanceled, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&roomv1.HookResponse{Output: out}), nil
}

// ServeUnix serves the API on a Unix socket with mode 0660 and group gid
// (-1 leaves the group), until ctx ends.
func ServeUnix(ctx context.Context, path string, gid int, h http.Handler) error {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return err
	}
	if gid >= 0 && os.Getuid() == 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			l.Close()
			return err
		}
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler returns the HTTP handler for the room API.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	path, h := roomv1connect.NewSupervisorServiceHandler(a)
	mux.Handle(path, h)
	return mux
}
