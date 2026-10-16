package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/wizard"
)

type workspaceSvc struct{ *Server }

func summary(w *v1alpha1.Workspace, rooms int) *ccv1.WorkspaceSummary {
	return &ccv1.WorkspaceSummary{Name: w.Name, DisplayName: w.Spec.DisplayName, Org: w.Spec.Forge.Org, Rooms: int32(rooms),
		Owners: w.Spec.Owners, Conditions: conditions(w.Status.Conditions), Namespace: w.Status.Namespace}
}

func (s workspaceSvc) ListWorkspaces(ctx context.Context, _ *connect.Request[ccv1.ListWorkspacesRequest]) (*connect.Response[ccv1.ListWorkspacesResponse], error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	wss, err := s.visibleWorkspaces(ctx, p)
	if err != nil {
		return nil, err
	}
	out := &ccv1.ListWorkspacesResponse{}
	for i := range wss {
		rooms, _ := s.K.Rooms(ctx, wss[i].Name)
		out.Workspaces = append(out.Workspaces, summary(&wss[i], len(rooms)))
	}
	return connect.NewResponse(out), nil
}

func (s workspaceSvc) GetWorkspace(ctx context.Context, req *connect.Request[ccv1.GetWorkspaceRequest]) (*connect.Response[ccv1.GetWorkspaceResponse], error) {
	if _, err := s.canView(ctx, req.Msg.GetName()); err != nil {
		return nil, err
	}
	cur, err := wizard.LoadCurrent(ctx, s.K.C, req.Msg.GetName())
	if err != nil {
		return nil, kerr(err)
	}
	if cur.Workspace == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such workspace"))
	}
	out := &ccv1.GetWorkspaceResponse{Plan: wizard.FromCluster(cur), Summary: summary(cur.Workspace, len(cur.Rooms))}
	for _, r := range s.Fleet.Rooms(func(ws string) bool { return ws == req.Msg.GetName() }) {
		out.Rooms = append(out.Rooms, r)
	}
	return connect.NewResponse(out), nil
}

// usedNames returns room names in use anywhere on the platform, except in skip.
func (s *Server) usedNames(ctx context.Context, skip string) map[string]bool {
	used := map[string]bool{}
	wss, _ := s.K.Workspaces(ctx)
	for _, w := range wss {
		if w.Name == skip {
			continue
		}
		rooms, _ := s.K.Rooms(ctx, w.Name)
		for _, r := range rooms {
			used[r.Name] = true
		}
	}
	return used
}

func (s workspaceSvc) SuggestAgentNames(ctx context.Context, req *connect.Request[ccv1.SuggestAgentNamesRequest]) (*connect.Response[ccv1.SuggestAgentNamesResponse], error) {
	if _, err := requireRole(ctx, auth.RoleOperator); err != nil {
		return nil, err
	}
	n := int(req.Msg.GetCount())
	if n <= 0 || n > 100 {
		return nil, errInvalid("count must be 1 to 100")
	}
	used := s.usedNames(ctx, "")
	for _, e := range req.Msg.GetExclude() {
		used[e] = true
	}
	used["forge"], used["status"] = true, true
	return connect.NewResponse(&ccv1.SuggestAgentNamesResponse{Names: wizard.Suggest(n, used), Roles: wizard.DefaultRoles(n)}), nil
}

var roleTitles = map[string][2]string{
	"coordinator": {"Coordinator", "Plans the work, splits it into issues and keeps the team moving."},
	"implementer": {"Implementer", "Takes issues, writes the code and opens pull requests."},
	"reviewer":    {"Reviewer", "Reviews every pull request against the task card."},
	"ci":          {"CI and infrastructure", "Owns the workflows, runners and build health."},
	"qa":          {"QA", "Tests the product end to end and files what breaks."},
	"chronicler":  {"Chronicler", "Reports on the swarm every few hours: what shipped, what's in flight, what's blocked."},
}

func (s workspaceSvc) ListRoleTemplates(ctx context.Context, req *connect.Request[ccv1.ListRoleTemplatesRequest]) (*connect.Response[ccv1.ListRoleTemplatesResponse], error) {
	out := &ccv1.ListRoleTemplatesResponse{}
	builtin := append(append([]string{}, wizard.BuiltinRoles...), "chronicler")
	if ws := req.Msg.GetWorkspace(); ws != "" {
		if _, err := s.canView(ctx, ws); err != nil {
			return nil, err
		}
		ts, err := s.K.RoleTemplates(ctx, ws)
		if err != nil {
			return nil, kerr(err)
		}
		for _, t := range ts {
			out.Templates = append(out.Templates, &ccv1.RoleTemplateSummary{Name: t.Name, Title: t.Spec.Title, Summary: t.Spec.Summary,
				BuiltIn: slices.Contains(builtin, t.Name)})
		}
		if len(out.Templates) > 0 {
			return connect.NewResponse(out), nil
		}
	} else if _, err := signedIn(ctx); err != nil {
		return nil, err
	}
	for _, n := range builtin {
		out.Templates = append(out.Templates, &ccv1.RoleTemplateSummary{Name: n, Title: roleTitles[n][0], Summary: roleTitles[n][1], BuiltIn: true})
	}
	return connect.NewResponse(out), nil
}

func (s workspaceSvc) ListModels(ctx context.Context, _ *connect.Request[ccv1.ListModelsRequest]) (*connect.Response[ccv1.ListModelsResponse], error) {
	if _, err := signedIn(ctx); err != nil {
		return nil, err
	}
	p, err := s.K.Platform(ctx)
	if err != nil {
		return nil, kerr(err)
	}
	out := &ccv1.ListModelsResponse{}
	for _, pr := range p.Spec.Services.Gateway.Providers {
		for _, m := range pr.Models {
			if !slices.Contains(out.Models, m) {
				out.Models = append(out.Models, m)
			}
		}
	}
	return connect.NewResponse(out), nil
}

// plan validates a plan for the caller and computes the changes.
func (s workspaceSvc) plan(ctx context.Context, p *ccv1.WorkspacePlan, prune bool) (*auth.Principal, wizard.Current, wizard.Desired, []*ccv1.Change, []string, error) {
	pr, err := requireRole(ctx, auth.RoleOperator)
	if err != nil {
		return nil, wizard.Current{}, wizard.Desired{}, nil, nil, err
	}
	if p == nil {
		return nil, wizard.Current{}, wizard.Desired{}, nil, nil, errInvalid("plan is required")
	}
	cur, err := wizard.LoadCurrent(ctx, s.K.C, p.GetName())
	if err != nil {
		return nil, cur, wizard.Desired{}, nil, nil, kerr(err)
	}
	if cur.Workspace != nil && !pr.CanOperate(cur.Workspace.Name, cur.Workspace.Spec.Owners) {
		return nil, cur, wizard.Desired{}, nil, nil, errForbidden("change workspace " + p.GetName())
	}
	// Whoever creates a workspace owns it.
	if cur.Workspace == nil && pr.Role < auth.RoleAdmin || cur.Workspace == nil && len(p.GetOwners()) == 0 {
		me := pr.Email
		if me == "" {
			me = pr.Handle
		}
		if !slices.ContainsFunc(p.GetOwners(), func(o string) bool { return strings.EqualFold(o, me) }) {
			p.Owners = append(p.Owners, me)
		}
	}
	problems := wizard.Validate(p, s.usedNames(ctx, p.GetName()))
	d := wizard.Build(p, cur)
	return pr, cur, d, wizard.Diff(cur, d, prune), problems, nil
}

func (s workspaceSvc) PreviewWorkspace(ctx context.Context, req *connect.Request[ccv1.PreviewWorkspaceRequest]) (*connect.Response[ccv1.PreviewWorkspaceResponse], error) {
	_, _, d, changes, problems, err := s.plan(ctx, req.Msg.GetPlan(), req.Msg.GetPruneRooms())
	if err != nil {
		return nil, err
	}
	out := &ccv1.PreviewWorkspaceResponse{Changes: changes, Problems: problems, Fits: true}
	if p, err := s.K.Platform(ctx); err == nil && p.Status.Preflight != nil && p.Status.Preflight.RoomEstimate > 0 {
		total := len(d.Rooms) + len(s.usedNames(ctx, req.Msg.GetPlan().GetName()))
		est := int(p.Status.Preflight.RoomEstimate)
		out.Fits = total <= est
		out.FitNote = fmt.Sprintf("%d rooms planned on the platform; preflight estimates room for about %d at the default size", total, est)
	}
	return connect.NewResponse(out), nil
}

func (s workspaceSvc) ApplyWorkspace(ctx context.Context, req *connect.Request[ccv1.ApplyWorkspaceRequest]) (*connect.Response[ccv1.ApplyWorkspaceResponse], error) {
	pr, cur, d, changes, problems, err := s.plan(ctx, req.Msg.GetPlan(), req.Msg.GetPruneRooms())
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, errInvalid("%s", strings.Join(problems, "; "))
	}
	ws := req.Msg.GetPlan().GetName()
	// Notification channel settings go to Secrets in the system namespace.
	for _, c := range req.Msg.GetPlan().GetNotificationChannels() {
		data := map[string][]byte{}
		switch c.GetKind() {
		case "slack", "discord":
			if c.GetWebhookUrl() != "" {
				data["url"] = []byte(c.GetWebhookUrl())
			}
		case "email":
			if c.GetSmtpHost() != "" {
				port := c.GetSmtpPort()
				if port == 0 {
					port = 587
				}
				data = map[string][]byte{"host": []byte(c.GetSmtpHost()), "port": []byte(fmt.Sprint(port)), "username": []byte(c.GetSmtpUsername()),
					"password": []byte(c.GetSmtpPassword()), "from": []byte(c.GetEmailFrom()), "to": []byte(c.GetEmailTo())}
			}
		}
		if len(data) > 0 {
			if err := s.K.ApplySecret(ctx, wizard.SecretName(ws, c.GetKind()), data); err != nil {
				return nil, kerr(err)
			}
		}
	}
	err = wizard.Apply(ctx, s.K.C, cur, d, changes, 20*time.Second)
	var summary []string
	for _, c := range changes {
		if c.Action != ccv1.ChangeAction_CHANGE_ACTION_UNCHANGED {
			summary = append(summary, fmt.Sprintf("%s %s/%s %v", c.Action, c.Kind, c.Name, c.Fields))
		}
	}
	s.audit(ctx, req, "workspace.apply", "workspace/"+ws, ws, nil, summary, err)
	if errors.Is(err, wizard.ErrNamespaceNotReady) {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if err != nil {
		return nil, kerr(err)
	}
	if cur.Workspace == nil {
		if db := s.DB(); db != nil && pr.UserID != "" {
			_ = db.AddOwner(ctx, pr.UserID, ws)
		}
	}
	s.Fleet.Kick()
	return connect.NewResponse(&ccv1.ApplyWorkspaceResponse{Changes: changes}), nil
}

var _ = k8s.PodName
