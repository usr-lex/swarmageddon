package api

import (
	"context"
	"errors"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/approvals"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

// ---------------------------------------------------------------- approvals

type approvalSvc struct{ *Server }

func approvalProto(a approvals.Approval) *ccv1.Approval {
	return &ccv1.Approval{Id: a.ID, Workspace: a.Workspace, Room: a.Room, Cli: a.CLI, Tool: a.Tool, Summary: a.Summary, Detail: a.Detail,
		Reason: a.Reason, SessionScopeAvailable: a.SessionScopeAvailable, RequestedAt: ts(a.RequestedAt), ExpiresAt: tsp(a.ExpiresAt),
		MatchingRules: a.MatchingRules, State: a.State, Decision: ccv1.Decision(approvals.DecisionFromName(a.Decision)),
		AnsweredBy: a.AnsweredBy, AnswerMessage: a.AnswerMessage, AnsweredAt: tsp(a.AnsweredAt)}
}

func wantSet(ws []string) func(string) bool {
	return func(w string) bool { return len(ws) == 0 || slices.Contains(ws, w) }
}

func (s approvalSvc) ListApprovals(ctx context.Context, req *connect.Request[ccv1.ListApprovalsRequest]) (*connect.Response[ccv1.ListApprovalsResponse], error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	allowed, want := s.viewFilter(ctx, p), wantSet(req.Msg.GetWorkspaces())
	out := &ccv1.ListApprovalsResponse{}
	for _, a := range s.Approvals.Pending(func(ws string) bool { return allowed(ws) && want(ws) }) {
		out.Approvals = append(out.Approvals, approvalProto(a))
	}
	return connect.NewResponse(out), nil
}

func (s approvalSvc) AnswerApproval(ctx context.Context, req *connect.Request[ccv1.AnswerApprovalRequest]) (*connect.Response[ccv1.AnswerApprovalResponse], error) {
	a, ok := s.Approvals.Get(req.Msg.GetId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, approvals.ErrNotPending)
	}
	p, err := s.canOperate(ctx, a.Workspace)
	if err != nil {
		return nil, err
	}
	by := p.Handle + "@" + a.Workspace
	res, err := s.Approvals.Answer(ctx, a.ID, by, messagingv1.Decision(req.Msg.GetDecision()), req.Msg.GetMessage())
	s.audit(ctx, req, "approval.answer", "approval/"+a.ID, a.Workspace, nil,
		map[string]string{"room": a.Room, "tool": a.Tool, "summary": a.Summary, "decision": req.Msg.GetDecision().String()}, err)
	if err != nil {
		if errors.Is(err, approvals.ErrNotPending) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, err
	}
	s.Fleet.ApprovalChanged(a.Workspace, a.Room)
	return connect.NewResponse(&ccv1.AnswerApprovalResponse{Approval: approvalProto(res)}), nil
}

func (s approvalSvc) WatchApprovals(ctx context.Context, req *connect.Request[ccv1.WatchApprovalsRequest], stream *connect.ServerStream[ccv1.WatchApprovalsResponse]) error {
	p, err := signedIn(ctx)
	if err != nil {
		return err
	}
	allowed, want := s.viewFilter(ctx, p), wantSet(req.Msg.GetWorkspaces())
	ch, cancel := s.Approvals.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case a, ok := <-ch:
			if !ok {
				return nil
			}
			if !allowed(a.Workspace) || !want(a.Workspace) {
				continue
			}
			if err := stream.Send(&ccv1.WatchApprovalsResponse{Approval: approvalProto(a)}); err != nil {
				return err
			}
		}
	}
}

func (s approvalSvc) ListApprovalHistory(ctx context.Context, req *connect.Request[ccv1.ListApprovalHistoryRequest]) (*connect.Response[ccv1.ListApprovalHistoryResponse], error) {
	db := s.DB()
	if db == nil {
		return nil, errNoDB()
	}
	var wss []string
	if ws := req.Msg.GetWorkspace(); ws != "" {
		if _, err := s.canView(ctx, ws); err != nil {
			return nil, err
		}
		wss = []string{ws}
	} else {
		p, err := signedIn(ctx)
		if err != nil {
			return nil, err
		}
		if p.Role != auth.RoleAdmin {
			vis, err := s.visibleWorkspaces(ctx, p)
			if err != nil {
				return nil, err
			}
			wss = []string{}
			for _, w := range vis {
				wss = append(wss, w.Name)
			}
		}
	}
	var before time.Time
	if req.Msg.GetBefore() != nil {
		before = req.Msg.GetBefore().AsTime()
	}
	list, err := db.ListApprovals(ctx, wss, "", before, int(req.Msg.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := &ccv1.ListApprovalHistoryResponse{}
	for _, a := range list {
		out.Approvals = append(out.Approvals, approvalProto(approvals.Approval{Approval: a}))
	}
	return connect.NewResponse(out), nil
}

// ---------------------------------------------------------------- audit

type auditSvc struct{ *Server }

func (s auditSvc) QueryAudit(ctx context.Context, req *connect.Request[ccv1.QueryAuditRequest]) (*connect.Response[ccv1.QueryAuditResponse], error) {
	m := req.Msg
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	var since, until time.Time
	if m.GetSince() != nil {
		since = m.GetSince().AsTime()
	}
	if m.GetUntil() != nil {
		until = m.GetUntil().AsTime()
	}
	out := &ccv1.QueryAuditResponse{}
	switch m.GetSource() {
	case ccv1.AuditSource_AUDIT_SOURCE_MESSAGING:
		if _, err := s.canView(ctx, m.GetWorkspace()); err != nil {
			return nil, err
		}
		if s.Msg == nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("messaging is not connected"))
		}
		owners, _ := s.workspaceOwners(ctx, m.GetWorkspace())
		bodies := p.CanOperate(m.GetWorkspace(), owners) // viewers get metadata only
		q := &messagingv1.QueryAuditRequest{Workspace: m.GetWorkspace(), Kinds: m.GetActions(), Address: m.GetActor(), Limit: m.GetLimit()}
		if !since.IsZero() {
			q.Since = timestamppb.New(since)
		}
		if !until.IsZero() {
			q.Until = timestamppb.New(until)
		}
		stream, err := s.Msg.QueryAudit(ctx, connect.NewRequest(q))
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		for stream.Receive() && len(out.Entries) < 5000 {
			r := stream.Msg()
			e := &ccv1.AuditEntry{Source: ccv1.AuditSource_AUDIT_SOURCE_MESSAGING, At: r.GetAt(), Actor: r.GetFrom(), Action: r.GetKind(),
				Object: r.GetTo(), Workspace: m.GetWorkspace(), Detail: jsonString(map[string]string{"messageId": r.GetMessageId(), "reason": r.GetReason(), "link": r.GetLink()})}
			if bodies {
				e.Detail = jsonString(map[string]string{"messageId": r.GetMessageId(), "reason": r.GetReason(), "link": r.GetLink(), "body": r.GetBody()})
			}
			out.Entries = append(out.Entries, e)
		}
		if err := stream.Err(); err != nil {
			return nil, err
		}
	default:
		db := s.DB()
		f := store.AuditFilter{Since: since, Until: until, Actor: m.GetActor(), Actions: m.GetActions(), Limit: int(m.GetLimit())}
		switch {
		case m.GetWorkspace() != "":
			if _, err := s.canView(ctx, m.GetWorkspace()); err != nil {
				return nil, err
			}
			f.Workspaces = []string{m.GetWorkspace()}
		case p.Role != auth.RoleAdmin:
			vis, err := s.visibleWorkspaces(ctx, p)
			if err != nil {
				return nil, err
			}
			f.Workspaces = []string{}
			for _, w := range vis {
				f.Workspaces = append(f.Workspaces, w.Name)
			}
		}
		var evs []store.AuditEvent
		if db != nil {
			if evs, err = db.QueryAudit(ctx, f); err != nil {
				return nil, err
			}
		} else if p.Role == auth.RoleAdmin {
			evs = s.Audit.Pending() // bootstrap phase: what is in memory
			slices.Reverse(evs)
		}
		for _, e := range evs {
			out.Entries = append(out.Entries, &ccv1.AuditEntry{Source: ccv1.AuditSource_AUDIT_SOURCE_CONTROL_CENTER, At: ts(e.At), Actor: e.Actor,
				Action: e.Action, Object: e.Object, Workspace: e.Workspace, SourceIp: e.SourceIP, Result: e.Result, Before: e.Before, After: e.After, Detail: e.Detail})
		}
	}
	return connect.NewResponse(out), nil
}

// ---------------------------------------------------------------- notifications

type notificationSvc struct{ *Server }

// vapid returns the web push keys from Secret vapid, creating them if missing.
func (s *Server) vapid(ctx context.Context) (*notify.VAPID, error) {
	d, err := s.K.GetSecret(ctx, k8s.SecretVAPID)
	if err != nil {
		if !isNotFound(err) {
			return nil, err
		}
		priv, pub, err := notify.GenerateVAPID()
		if err != nil {
			return nil, err
		}
		if d, err = s.K.CreateSecretIfMissing(ctx, k8s.SecretVAPID, map[string][]byte{"publicKey": []byte(pub), "privateKey": []byte(priv)}); err != nil {
			return nil, err
		}
	}
	sub := "mailto:admin@swarmageddon.invalid"
	if s.Cfg.PublicURL != "" {
		sub = s.Cfg.PublicURL
	}
	return &notify.VAPID{PublicKey: string(d["publicKey"]), PrivateKey: string(d["privateKey"]), Subscriber: sub}, nil
}

func (s notificationSvc) GetVapidPublicKey(ctx context.Context, _ *connect.Request[ccv1.GetVapidPublicKeyRequest]) (*connect.Response[ccv1.GetVapidPublicKeyResponse], error) {
	if _, err := signedIn(ctx); err != nil {
		return nil, err
	}
	v, err := s.vapid(ctx)
	if err != nil {
		return nil, kerr(err)
	}
	return connect.NewResponse(&ccv1.GetVapidPublicKeyResponse{PublicKey: v.PublicKey}), nil
}

func (s notificationSvc) db(ctx context.Context) (*auth.Principal, *store.Store, error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, nil, err
	}
	db := s.DB()
	if db == nil || p.UserID == "" {
		return nil, nil, errNoDB()
	}
	return p, db, nil
}

func (s notificationSvc) SubscribeWebPush(ctx context.Context, req *connect.Request[ccv1.SubscribeWebPushRequest]) (*connect.Response[ccv1.SubscribeWebPushResponse], error) {
	p, db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if len(m.GetEndpoint()) < 10 || len(m.GetEndpoint()) > 2048 || m.GetP256Dh() == "" || m.GetAuth() == "" {
		return nil, errInvalid("endpoint, p256dh and auth are required")
	}
	if err := db.SavePushSubscription(ctx, store.PushSubscription{Endpoint: m.GetEndpoint(), UserID: p.UserID, P256dh: m.GetP256Dh(), Auth: m.GetAuth(), UserAgent: m.GetUserAgent()}); err != nil {
		return nil, err
	}
	s.audit(ctx, req, "notify.subscribe", "user/"+p.UserID, "", nil, nil, nil)
	return connect.NewResponse(&ccv1.SubscribeWebPushResponse{}), nil
}

func (s notificationSvc) UnsubscribeWebPush(ctx context.Context, req *connect.Request[ccv1.UnsubscribeWebPushRequest]) (*connect.Response[ccv1.UnsubscribeWebPushResponse], error) {
	p, db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	if err := db.DeletePushSubscription(ctx, p.UserID, req.Msg.GetEndpoint()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.UnsubscribeWebPushResponse{}), nil
}

var notificationEvents = []string{"approvalWaiting", "idleWithOpenWork", "ciRed", "budget", "diskLow", "roomCrashed"}

func (s notificationSvc) GetNotificationPrefs(ctx context.Context, _ *connect.Request[ccv1.GetNotificationPrefsRequest]) (*connect.Response[ccv1.GetNotificationPrefsResponse], error) {
	p, db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	prefs, err := db.Prefs(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := &ccv1.GetNotificationPrefsResponse{AvailableChannels: []string{"webPush"}}
	for _, pr := range prefs {
		out.Prefs = append(out.Prefs, &ccv1.NotificationPref{Workspace: pr.Workspace, Event: pr.Event, Channels: pr.Channels})
	}
	if vis, err := s.visibleWorkspaces(ctx, p); err == nil {
		for _, w := range vis {
			for _, c := range w.Spec.Notifications.Channels {
				if !slices.Contains(out.AvailableChannels, string(c.Kind)) {
					out.AvailableChannels = append(out.AvailableChannels, string(c.Kind))
				}
			}
		}
	}
	return connect.NewResponse(out), nil
}

func (s notificationSvc) SetNotificationPrefs(ctx context.Context, req *connect.Request[ccv1.SetNotificationPrefsRequest]) (*connect.Response[ccv1.SetNotificationPrefsResponse], error) {
	p, db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	var prefs []store.Pref
	for _, pr := range req.Msg.GetPrefs() {
		if !slices.Contains(notificationEvents, pr.GetEvent()) {
			return nil, errInvalid("unknown event %q", pr.GetEvent())
		}
		prefs = append(prefs, store.Pref{Workspace: pr.GetWorkspace(), Event: pr.GetEvent(), Channels: pr.GetChannels()})
	}
	if err := db.SetPrefs(ctx, p.UserID, prefs); err != nil {
		return nil, err
	}
	s.audit(ctx, req, "notify.prefs", "user/"+p.UserID, "", nil, prefs, nil)
	return connect.NewResponse(&ccv1.SetNotificationPrefsResponse{}), nil
}

func (s notificationSvc) SendTestNotification(ctx context.Context, req *connect.Request[ccv1.SendTestNotificationRequest]) (*connect.Response[ccv1.SendTestNotificationResponse], error) {
	p, db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	if c := req.Msg.GetChannel(); c != "" && c != "webPush" {
		return nil, errInvalid("only webPush can be tested per person; workspace channels are tested from the workspace")
	}
	subs, err := db.PushSubscriptions(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this browser hasn't subscribed to notifications"))
	}
	if err := s.Notify.SendTest(ctx, subs); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&ccv1.SendTestNotificationResponse{}), nil
}

// notifyDir implements notify.Directory.
type notifyDir struct{ s *Server }

func (d notifyDir) Settings(ctx context.Context, ws string) (v1alpha1.NotificationSettings, error) {
	w, err := d.s.K.Workspace(ctx, ws)
	if err != nil {
		return v1alpha1.NotificationSettings{}, err
	}
	return w.Spec.Notifications, nil
}

func (d notifyDir) Recipients(ctx context.Context, ws string, kind v1alpha1.NotificationEvent) ([]notify.Recipient, error) {
	db := d.s.DB()
	if db == nil {
		return nil, nil
	}
	w, err := d.s.K.Workspace(ctx, ws)
	if err != nil {
		return nil, err
	}
	users, err := db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []notify.Recipient
	for _, u := range users {
		if u.Disabled {
			continue
		}
		assigned, _ := db.Assignments(ctx, u.ID)
		p := &auth.Principal{Handle: u.Handle, Email: u.Email, Role: auth.ParseRole(u.Role()), Workspaces: assigned}
		if !p.CanView(ws, w.Spec.Owners) {
			continue
		}
		subs, _ := db.PushSubscriptions(ctx, u.ID)
		if len(subs) == 0 {
			continue
		}
		prefs, _ := db.Prefs(ctx, u.ID)
		channels := []string{"webPush"} // default: browser push for every event
		for _, pr := range prefs {
			if pr.Event == string(kind) && pr.Workspace == "" {
				channels = pr.Channels
			}
		}
		for _, pr := range prefs {
			if pr.Event == string(kind) && pr.Workspace == ws {
				channels = pr.Channels
			}
		}
		out = append(out, notify.Recipient{UserID: u.ID, Channels: channels, Push: subs})
	}
	return out, nil
}

func (d notifyDir) Secret(ctx context.Context, name string) (map[string][]byte, error) {
	return d.s.K.GetSecret(ctx, name)
}

func (d notifyDir) DropPush(ctx context.Context, endpoint string) {
	if db := d.s.DB(); db != nil {
		_ = db.DeletePushSubscription(ctx, "", endpoint)
	}
}

// NotifyDirectory returns the notification directory backed by this server.
func (s *Server) NotifyDirectory() notify.Directory { return notifyDir{s} }

// ---------------------------------------------------------------- spend

type spendSvc struct{ *Server }

func (s spendSvc) GetSpend(ctx context.Context, req *connect.Request[ccv1.GetSpendRequest]) (*connect.Response[ccv1.GetSpendResponse], error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	allowed := s.viewFilter(ctx, p)
	want := req.Msg.GetWorkspace()
	snap := s.Spend.Latest()
	out := &ccv1.GetSpendResponse{UpdatedAt: ts(snap.UpdatedAt), Available: !snap.UpdatedAt.IsZero()}
	per := map[string]*ccv1.WorkspaceSpend{}
	models := map[string]map[string]float64{}
	for _, k := range snap.Rooms {
		if !allowed(k.Workspace) || (want != "" && k.Workspace != want) {
			continue
		}
		budget := k.MaxBudget
		if budget <= 0 {
			if w, err := s.K.Workspace(ctx, k.Workspace); err == nil {
				budget = float64(w.Spec.Models.Budgets.PerRoomMonthlyUSD)
			}
		}
		out.Rooms = append(out.Rooms, &ccv1.RoomSpend{Workspace: k.Workspace, Room: k.Room, SpendUsd: k.Spend, BudgetUsd: budget,
			BudgetExhausted: budget > 0 && k.Spend >= budget})
		ws := per[k.Workspace]
		if ws == nil {
			ws = &ccv1.WorkspaceSpend{Workspace: k.Workspace}
			per[k.Workspace] = ws
			models[k.Workspace] = map[string]float64{}
		}
		ws.SpendUsd += k.Spend
		for m, v := range k.ModelSpend {
			models[k.Workspace][m] += v
		}
	}
	for name, ws := range per {
		for m, v := range models[name] {
			ws.Models = append(ws.Models, &ccv1.ModelSpend{Model: m, SpendUsd: v})
		}
		out.Workspaces = append(out.Workspaces, ws)
	}
	return connect.NewResponse(out), nil
}

// ---------------------------------------------------------------- users

type userSvc struct{ *Server }

func (s userSvc) admin(ctx context.Context) (*store.Store, error) {
	if _, err := requireRole(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	db := s.DB()
	if db == nil {
		return nil, errNoDB()
	}
	return db, nil
}

func (s userSvc) userResp(ctx context.Context, db *store.Store, id string) (*ccv1.User, error) {
	u, err := db.GetUser(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	ws, _ := db.Assignments(ctx, id)
	return storeUserProto(u, ws), nil
}

func (s userSvc) ListUsers(ctx context.Context, _ *connect.Request[ccv1.ListUsersRequest]) (*connect.Response[ccv1.ListUsersResponse], error) {
	db, err := s.admin(ctx)
	if err != nil {
		return nil, err
	}
	users, err := db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := &ccv1.ListUsersResponse{}
	for _, u := range users {
		ws, _ := db.Assignments(ctx, u.ID)
		out.Users = append(out.Users, storeUserProto(u, ws))
	}
	return connect.NewResponse(out), nil
}

func (s userSvc) SetUserRole(ctx context.Context, req *connect.Request[ccv1.SetUserRoleRequest]) (*connect.Response[ccv1.SetUserRoleResponse], error) {
	db, err := s.admin(ctx)
	if err != nil {
		return nil, err
	}
	before, _ := db.GetUser(ctx, req.Msg.GetUserId())
	err = db.SetRoleOverride(ctx, req.Msg.GetUserId(), string(roleFromProto(req.Msg.GetRole())))
	s.audit(ctx, req, "user.role", "user/"+req.Msg.GetUserId(), "", before.Role(), req.Msg.GetRole().String(), err)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	u, err := s.userResp(ctx, db, req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.SetUserRoleResponse{User: u}), nil
}

func (s userSvc) SetUserWorkspaces(ctx context.Context, req *connect.Request[ccv1.SetUserWorkspacesRequest]) (*connect.Response[ccv1.SetUserWorkspacesResponse], error) {
	db, err := s.admin(ctx)
	if err != nil {
		return nil, err
	}
	before, _ := db.Assignments(ctx, req.Msg.GetUserId())
	err = db.SetAssignments(ctx, req.Msg.GetUserId(), req.Msg.GetWorkspaces())
	s.audit(ctx, req, "user.workspaces", "user/"+req.Msg.GetUserId(), "", before, req.Msg.GetWorkspaces(), err)
	if err != nil {
		return nil, err
	}
	u, err := s.userResp(ctx, db, req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.SetUserWorkspacesResponse{User: u}), nil
}

func (s userSvc) SetUserDisabled(ctx context.Context, req *connect.Request[ccv1.SetUserDisabledRequest]) (*connect.Response[ccv1.SetUserDisabledResponse], error) {
	db, err := s.admin(ctx)
	if err != nil {
		return nil, err
	}
	if p := auth.FromContext(ctx); p.UserID == req.Msg.GetUserId() && req.Msg.GetDisabled() {
		return nil, errInvalid("you can't disable yourself")
	}
	err = db.SetDisabled(ctx, req.Msg.GetUserId(), req.Msg.GetDisabled())
	s.audit(ctx, req, "user.disabled", "user/"+req.Msg.GetUserId(), "", nil, req.Msg.GetDisabled(), err)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	u, err := s.userResp(ctx, db, req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.SetUserDisabledResponse{User: u}), nil
}
