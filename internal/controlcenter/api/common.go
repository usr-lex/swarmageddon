package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

func errUnauthenticated() error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
}

func errForbidden(what string) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("you may not %s", what))
}

func errInvalid(format string, a ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, a...))
}

func errNoDB() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("this needs PostgreSQL, which is not deployed yet"))
}

// kerr maps a Kubernetes error to a Connect error.
func kerr(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, err)
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case apierrors.IsForbidden(err):
		return connect.NewError(connect.CodePermissionDenied, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return err
}

// signedIn returns the principal or an error.
func signedIn(ctx context.Context) (*auth.Principal, error) {
	p := auth.FromContext(ctx)
	if p == nil || p.Role == auth.RoleNone {
		return nil, errUnauthenticated()
	}
	return p, nil
}

func requireRole(ctx context.Context, r auth.Role) (*auth.Principal, error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	if p.Role < r {
		return nil, errForbidden("do this with the " + p.Role.String() + " role")
	}
	return p, nil
}

// canView checks that the principal may see a workspace.
func (s *Server) canView(ctx context.Context, ws string) (*auth.Principal, error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	if ws == "" {
		return nil, errInvalid("workspace is required")
	}
	owners, err := s.workspaceOwners(ctx, ws)
	if err != nil {
		return nil, kerr(err)
	}
	if !p.CanView(ws, owners) {
		return nil, errForbidden("see workspace " + ws)
	}
	return p, nil
}

// canOperate checks that the principal may change a workspace.
func (s *Server) canOperate(ctx context.Context, ws string) (*auth.Principal, error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	owners, err := s.workspaceOwners(ctx, ws)
	if err != nil {
		return nil, kerr(err)
	}
	if !p.CanOperate(ws, owners) {
		return nil, errForbidden("change workspace " + ws)
	}
	return p, nil
}

// visibleWorkspaces returns the workspaces the principal may see.
func (s *Server) visibleWorkspaces(ctx context.Context, p *auth.Principal) ([]v1alpha1.Workspace, error) {
	all, err := s.K.Workspaces(ctx)
	if err != nil {
		return nil, kerr(err)
	}
	var out []v1alpha1.Workspace
	for _, w := range all {
		if p.CanView(w.Name, w.Spec.Owners) {
			out = append(out, w)
		}
	}
	return out, nil
}

// viewFilter returns a func telling visible workspaces.
func (s *Server) viewFilter(ctx context.Context, p *auth.Principal) func(string) bool {
	ws, _ := s.visibleWorkspaces(ctx, p)
	set := map[string]bool{}
	for _, w := range ws {
		set[w.Name] = true
	}
	return func(name string) bool { return set[name] }
}

func clientIP(_ context.Context, h http.Header, peer string) string {
	return session.ClientIP(&http.Request{RemoteAddr: peer, Header: h})
}

// auditCtx records an action by the principal in ctx.
func (s *Server) audit(ctx context.Context, req connect.AnyRequest, action, object, ws string, before, after any, result error) {
	ev := store.AuditEvent{Actor: auth.FromContext(ctx).Name(), Action: action, Object: object, Workspace: ws}
	if req != nil {
		ev.SourceIP = clientIP(ctx, req.Header(), req.Peer().Addr)
	}
	if before != nil {
		ev.Before = jsonString(before)
	}
	if after != nil {
		ev.After = jsonString(after)
	}
	if result != nil {
		ev.Result = result.Error()
	}
	s.Audit.Record(ctx, ev)
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// cookieWriter lets the session manager set cookies on a Connect response.
type cookieWriter struct{ h http.Header }

func (c cookieWriter) Header() http.Header         { return c.h }
func (c cookieWriter) Write(b []byte) (int, error) { return len(b), nil }
func (c cookieWriter) WriteHeader(int)             {}

// requestFor builds an *http.Request carrying a Connect request's headers,
// for the session manager.
func requestFor(req connect.AnyRequest) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/", nil)
	r.Header = req.Header().Clone()
	r.RemoteAddr = req.Peer().Addr
	return r
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tsp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func roleProto(r auth.Role) ccv1.Role {
	switch r {
	case auth.RoleAdmin:
		return ccv1.Role_ROLE_ADMIN
	case auth.RoleOperator:
		return ccv1.Role_ROLE_OPERATOR
	case auth.RoleViewer:
		return ccv1.Role_ROLE_VIEWER
	}
	return ccv1.Role_ROLE_UNSPECIFIED
}

func roleFromProto(r ccv1.Role) v1alpha1.Role {
	switch r {
	case ccv1.Role_ROLE_ADMIN:
		return v1alpha1.RoleAdmin
	case ccv1.Role_ROLE_OPERATOR:
		return v1alpha1.RoleOperator
	case ccv1.Role_ROLE_VIEWER:
		return v1alpha1.RoleViewer
	}
	return ""
}

func conditions(cs []metav1.Condition) []*ccv1.Condition {
	var out []*ccv1.Condition
	for _, c := range cs {
		out = append(out, &ccv1.Condition{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message,
			LastTransition: ts(c.LastTransitionTime.Time)})
	}
	return out
}

func presenceProto(p messagingv1.Presence) ccv1.Presence {
	return ccv1.Presence(p) // same numbering by design
}

func userProto(p *auth.Principal) *ccv1.User {
	return &ccv1.User{Id: p.UserID, Handle: p.Handle, Email: p.Email, DisplayName: p.DisplayName,
		Role: roleProto(p.Role), Workspaces: p.Workspaces, Provider: p.Provider}
}

func storeUserProto(u store.User, ws []string) *ccv1.User {
	return &ccv1.User{Id: u.ID, Handle: u.Handle, Email: u.Email, DisplayName: u.DisplayName,
		Role: roleProto(auth.ParseRole(u.Role())), Workspaces: ws, Provider: u.Provider, Disabled: u.Disabled,
		RoleOverride: u.RoleOverride != "", LastLogin: tsp(u.LastLogin)}
}

// noSender is used when messaging isn't configured (dev).
type noSender struct{}

func (noSender) SendAs(context.Context, string, string, string, string, string) (*messagingv1.SendResponse, error) {
	return nil, errors.New("messaging is not configured")
}
