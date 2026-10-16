package api

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
)

// channelPoll is how often WatchChannelItems reads each workspace.
var channelPoll = 2 * time.Second

// maxSeen bounds the ids a stream remembers per workspace.
const maxSeen = 5000

// WatchChannelItems polls messaging's ReadMessages for the person (unread,
// not marking anything read) in each workspace, and sends items it hasn't
// sent before. Messaging pushes nothing for pull-mode channels, so the
// person's view is read the same way an agent reads.
func (s channelSvc) WatchChannelItems(ctx context.Context, req *connect.Request[ccv1.WatchChannelItemsRequest], stream *connect.ServerStream[ccv1.WatchChannelItemsResponse]) error {
	p, err := signedIn(ctx)
	if err != nil {
		return err
	}
	if s.Msg == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("messaging is not connected"))
	}
	visible, err := s.visibleWorkspaces(ctx, p)
	if err != nil {
		return err
	}
	want := wantSet(req.Msg.GetWorkspaces())
	seen := map[string]map[string]bool{} // workspace -> item ids; nil until the first read
	var wss []string
	for _, w := range visible {
		if want(w.Name) {
			wss = append(wss, w.Name)
		}
	}
	if len(wss) == 0 {
		return errForbidden("see any of these workspaces")
	}
	t := time.NewTicker(channelPoll)
	defer t.Stop()
	for {
		failed := 0
		var lastErr error
		for _, ws := range wss {
			res, err := s.Msg.ReadMessages(ctx, onBehalf(&messagingv1.ReadMessagesRequest{Scope: "all", UnreadOnly: ptr(true),
				Limit: 50, Full: true, MarkRead: ptr(false)}, p.Handle+"@"+ws))
			if err != nil {
				failed++
				lastErr = err
				continue
			}
			first := seen[ws] == nil
			if first || len(seen[ws]) > maxSeen {
				// Start over from what is unread now; read items don't come back.
				old := seen[ws]
				seen[ws] = map[string]bool{}
				for _, it := range res.Msg.GetItems() {
					if old[it.GetId()] {
						seen[ws][it.GetId()] = true
					}
				}
			}
			for _, it := range res.Msg.GetItems() {
				if seen[ws][it.GetId()] {
					continue
				}
				seen[ws][it.GetId()] = true
				if first {
					continue
				}
				if err := stream.Send(&ccv1.WatchChannelItemsResponse{Workspace: ws, Item: itemProto(it)}); err != nil {
					return err
				}
			}
		}
		if failed == len(wss) && len(seen) == 0 {
			return lastErr // never read anything: say why
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func ptr[T any](v T) *T { return &v }

// ListMentionables lists agents and people of a workspace for @mentions.
func (s channelSvc) ListMentionables(ctx context.Context, req *connect.Request[ccv1.ListMentionablesRequest]) (*connect.Response[ccv1.ListMentionablesResponse], error) {
	ws := req.Msg.GetWorkspace()
	p, err := s.canView(ctx, ws)
	if err != nil {
		return nil, err
	}
	byHandle := map[string]*ccv1.Mentionable{}
	reachable := false
	if s.Msg != nil {
		res, err := s.Msg.ListPeers(ctx, onBehalf(&messagingv1.ListPeersRequest{}, p.Handle+"@"+ws))
		if err == nil {
			reachable = true
			for _, peer := range res.Msg.GetPeers() {
				h := peer.GetAddress()
				if name, w, ok := strings.Cut(h, "@"); ok && w == ws {
					h = name
				}
				kind := "agent"
				if peer.GetKind() == "human" {
					kind = "person"
				}
				byHandle[h] = &ccv1.Mentionable{Handle: h, Kind: kind, DisplayName: peer.GetDisplayName(), Role: peer.GetTitle(),
					Group: peer.GetGroup(), Presence: presenceProto(peer.GetPresence()), Reachable: true}
			}
		}
	}
	if !reachable {
		// Messaging is unavailable: the workspace's rooms, reachability unknown.
		for _, r := range s.Fleet.Rooms(func(w string) bool { return w == ws }) {
			byHandle[r.GetName()] = &ccv1.Mentionable{Handle: r.GetName(), Kind: "agent", DisplayName: r.GetDisplayName(),
				Role: r.GetRole(), Group: r.GetGroup(), Presence: r.GetPresence()}
		}
	}
	if db := s.DB(); db != nil {
		owners, _ := s.workspaceOwners(ctx, ws)
		users, err := db.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			if u.Disabled || u.Provider == "setup" || u.Handle == p.Handle {
				continue
			}
			assigned, _ := db.Assignments(ctx, u.ID)
			up := &auth.Principal{Handle: u.Handle, Email: u.Email, Role: auth.ParseRole(u.Role()), Workspaces: assigned}
			if !up.CanView(ws, owners) {
				continue
			}
			if m, ok := byHandle[u.Handle]; ok {
				if m.DisplayName == "" {
					m.DisplayName = u.DisplayName
				}
				continue
			}
			byHandle[u.Handle] = &ccv1.Mentionable{Handle: u.Handle, Kind: "person", DisplayName: u.DisplayName}
		}
	}
	delete(byHandle, p.Handle)
	out := &ccv1.ListMentionablesResponse{}
	for _, m := range byHandle {
		out.Mentionables = append(out.Mentionables, m)
	}
	sort.Slice(out.Mentionables, func(i, j int) bool {
		a, b := out.Mentionables[i], out.Mentionables[j]
		if a.Kind != b.Kind {
			return a.Kind == "agent"
		}
		return a.Handle < b.Handle
	})
	return connect.NewResponse(out), nil
}
