package api

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/approvals"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
)

// presenceInfo is what messaging last said about a room.
type presenceInfo struct {
	presence messagingv1.Presence
	since    time.Time
	status   *messagingv1.RoomStatusReport
}

// Fleet merges Rooms and their pods (polled from Kubernetes) with presence
// and status from messaging, and pushes changes to watchers (§8.4).
type Fleet struct {
	K         *k8s.Client
	Approvals *approvals.Manager
	// Spend returns current spend per workspace/room; optional.
	Spend func() map[string]float64
	// OnPodRestarts is told each room's container restart count; optional.
	OnPodRestarts func(ctx context.Context, ws, room string, restarts int32)
	// OnPresence is told each presence event; optional.
	OnPresence func(ctx context.Context, ws, room string, p messagingv1.Presence, at time.Time, st *messagingv1.RoomStatusReport)
	Interval   time.Duration
	Log        *slog.Logger

	mu       sync.Mutex
	rooms    map[string]*ccv1.RoomSummary // ws/room
	presence map[string]presenceInfo      // ws/room
	subs     map[chan *ccv1.WatchFleetResponse]struct{}
	kick     chan struct{}
}

// NewFleet returns an empty fleet.
func NewFleet(k *k8s.Client, a *approvals.Manager) *Fleet {
	return &Fleet{K: k, Approvals: a, rooms: map[string]*ccv1.RoomSummary{}, presence: map[string]presenceInfo{},
		subs: map[chan *ccv1.WatchFleetResponse]struct{}{}, kick: make(chan struct{}, 1), Log: slog.Default()}
}

// Kick asks for a refresh now.
func (f *Fleet) Kick() {
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

// Run refreshes from Kubernetes until ctx ends.
func (f *Fleet) Run(ctx context.Context) {
	iv := f.Interval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := f.Refresh(ctx); err != nil {
			f.Log.Debug("fleet refresh", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-f.kick:
		}
	}
}

// Refresh rebuilds the fleet from Kubernetes.
func (f *Fleet) Refresh(ctx context.Context) error {
	wss, err := f.K.Workspaces(ctx)
	if err != nil {
		return err
	}
	next := map[string]*ccv1.RoomSummary{}
	type restart struct {
		ws, room string
		n        int32
	}
	var restarts []restart
	for _, w := range wss {
		rooms, err := f.K.Rooms(ctx, w.Name)
		if err != nil {
			continue // namespace not ready yet, or no access
		}
		pods, _ := f.K.RoomPods(ctx, w.Name)
		for _, r := range rooms {
			s := &ccv1.RoomSummary{Workspace: w.Name, Name: r.Name, DisplayName: r.Spec.DisplayName, Role: r.Spec.Role, Cli: string(r.Spec.CLI),
				Model: r.Spec.Model, Group: r.Spec.Group, Phase: string(r.Status.Phase), UpdateAvailable: r.Status.UpdateAvailable,
				Paused: r.Spec.Paused, Conditions: conditions(r.Status.Conditions), Address: r.Status.Address}
			if s.Model == "" {
				s.Model = w.Spec.Models.Default
			}
			if s.Address == "" {
				s.Address = r.Name + "@" + w.Name
			}
			if p := pods[r.Name]; p != nil {
				for _, cs := range p.Status.ContainerStatuses {
					s.PodRestarts += cs.RestartCount
				}
				restarts = append(restarts, restart{w.Name, r.Name, s.PodRestarts})
			}
			next[w.Name+"/"+r.Name] = s
		}
	}
	if f.OnPodRestarts != nil {
		for _, r := range restarts {
			f.OnPodRestarts(ctx, r.ws, r.room, r.n)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pending := map[string]int{}
	if f.Approvals != nil {
		pending = f.Approvals.PendingCount()
	}
	var spend map[string]float64
	if f.Spend != nil {
		spend = f.Spend()
	}
	for key, s := range next {
		f.merge(key, s, pending, spend)
		if old, ok := f.rooms[key]; !ok || !proto.Equal(old, s) {
			f.broadcast(&ccv1.WatchFleetResponse{Event: &ccv1.WatchFleetResponse_Updated{Updated: s}})
		}
	}
	for key, old := range f.rooms {
		if _, ok := next[key]; !ok {
			f.broadcast(&ccv1.WatchFleetResponse{Event: &ccv1.WatchFleetResponse_Removed{Removed: &ccv1.RoomRef{Workspace: old.Workspace, Name: old.Name}}})
		}
	}
	f.rooms = next
	return nil
}

// merge adds presence, status, approvals and spend to a summary (f.mu held).
func (f *Fleet) merge(key string, s *ccv1.RoomSummary, pending map[string]int, spend map[string]float64) {
	if pi, ok := f.presence[key]; ok {
		s.Presence = presenceProto(pi.presence)
		s.PresenceSince = ts(pi.since)
		if st := pi.status; st != nil {
			s.CurrentTask = st.GetCurrentTask()
			s.CrashCount = st.GetCrashCount()
			s.Volumes = nil
			for _, v := range st.GetVolumes() {
				s.Volumes = append(s.Volumes, &ccv1.VolumeUsage{Name: v.GetName(), UsedBytes: v.GetUsedBytes(), CapacityBytes: v.GetCapacityBytes()})
			}
		}
	} else {
		s.Presence = ccv1.Presence_PRESENCE_OFFLINE
	}
	s.PendingApprovals = int32(pending[key])
	if spend != nil {
		s.SpendUsd = spend[key]
	}
}

// WatchPresence consumes one messaging presence stream until it ends.
func (f *Fleet) WatchPresence(ctx context.Context, msg interface {
	WatchPresence(context.Context, *connect.Request[messagingv1.WatchPresenceRequest]) (*connect.ServerStreamForClient[messagingv1.PresenceEvent], error)
}) error {
	stream, err := msg.WatchPresence(ctx, connect.NewRequest(&messagingv1.WatchPresenceRequest{}))
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		f.Presence(ctx, stream.Msg())
	}
	return stream.Err()
}

// Presence applies one presence event.
func (f *Fleet) Presence(ctx context.Context, ev *messagingv1.PresenceEvent) {
	room, ws := approvals.SplitAddress(ev.GetAddress())
	if ws == "" {
		return
	}
	at := time.Now().UTC()
	if ev.GetAt() != nil {
		at = ev.GetAt().AsTime()
	}
	key := ws + "/" + room
	f.mu.Lock()
	pi := f.presence[key]
	if pi.presence != ev.GetPresence() || pi.since.IsZero() {
		pi.since = at
	}
	pi.presence = ev.GetPresence()
	if ev.GetStatus() != nil {
		pi.status = ev.GetStatus()
	}
	f.presence[key] = pi
	if s, ok := f.rooms[key]; ok {
		u := proto.Clone(s).(*ccv1.RoomSummary)
		var spend map[string]float64
		if f.Spend != nil {
			spend = f.Spend()
		}
		pending := map[string]int{}
		if f.Approvals != nil {
			pending = f.Approvals.PendingCount()
		}
		f.merge(key, u, pending, spend)
		if !proto.Equal(s, u) {
			f.rooms[key] = u
			f.broadcast(&ccv1.WatchFleetResponse{Event: &ccv1.WatchFleetResponse_Updated{Updated: u}})
		}
	}
	f.mu.Unlock()
	if f.OnPresence != nil {
		f.OnPresence(ctx, ws, room, ev.GetPresence(), at, ev.GetStatus())
	}
}

// Status returns the latest status report of a room.
func (f *Fleet) Status(ws, room string) *messagingv1.RoomStatusReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.presence[ws+"/"+room].status
}

// Rooms returns the summaries visible through allow, sorted as the fleet
// view sorts them: waiting on a human, crashed, working, idle, offline.
func (f *Fleet) Rooms(allow func(ws string) bool) []*ccv1.RoomSummary {
	f.mu.Lock()
	var out []*ccv1.RoomSummary
	for _, s := range f.rooms {
		if allow == nil || allow(s.Workspace) {
			out = append(out, proto.Clone(s).(*ccv1.RoomSummary))
		}
	}
	f.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		a, b := rank(out[i]), rank(out[j])
		if a != b {
			return a < b
		}
		if out[i].Workspace != out[j].Workspace {
			return out[i].Workspace < out[j].Workspace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func rank(s *ccv1.RoomSummary) int {
	switch {
	case s.Presence == ccv1.Presence_PRESENCE_WAITING_ON_HUMAN || s.PendingApprovals > 0:
		return 0
	case s.Phase == string(v1alpha1.RoomFailed) || s.CrashCount >= 3:
		return 1
	case s.Presence == ccv1.Presence_PRESENCE_WORKING:
		return 2
	case s.Presence == ccv1.Presence_PRESENCE_IDLE:
		return 3
	}
	return 4
}

// Subscribe returns a channel of fleet changes.
func (f *Fleet) Subscribe() (<-chan *ccv1.WatchFleetResponse, func()) {
	ch := make(chan *ccv1.WatchFleetResponse, 256)
	f.mu.Lock()
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		if _, ok := f.subs[ch]; ok {
			delete(f.subs, ch)
			close(ch)
		}
		f.mu.Unlock()
	}
}

func (f *Fleet) broadcast(ev *ccv1.WatchFleetResponse) {
	for ch := range f.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ApprovalChanged refreshes pending counts after an approval changes.
func (f *Fleet) ApprovalChanged(ws, room string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := ws + "/" + room
	s, ok := f.rooms[key]
	if !ok {
		return
	}
	u := proto.Clone(s).(*ccv1.RoomSummary)
	u.PendingApprovals = int32(f.Approvals.PendingCount()[key])
	if !proto.Equal(s, u) {
		f.rooms[key] = u
		f.broadcast(&ccv1.WatchFleetResponse{Event: &ccv1.WatchFleetResponse_Updated{Updated: u}})
	}
}

type fleetSvc struct{ *Server }

func (s fleetSvc) WatchFleet(ctx context.Context, req *connect.Request[ccv1.WatchFleetRequest], stream *connect.ServerStream[ccv1.WatchFleetResponse]) error {
	p, err := signedIn(ctx)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, w := range req.Msg.GetWorkspaces() {
		want[w] = true
	}
	allowed := s.viewFilter(ctx, p)
	allow := func(ws string) bool { return allowed(ws) && (len(want) == 0 || want[ws]) }
	ch, cancel := s.Fleet.Subscribe()
	defer cancel()
	if err := stream.Send(&ccv1.WatchFleetResponse{Event: &ccv1.WatchFleetResponse_Snapshot{Snapshot: &ccv1.FleetSnapshot{Rooms: s.Fleet.Rooms(allow)}}}); err != nil {
		return err
	}
	refresh := time.NewTicker(time.Minute)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refresh.C:
			allowed = s.viewFilter(ctx, p) // assignments may change
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			ws := ""
			switch e := ev.Event.(type) {
			case *ccv1.WatchFleetResponse_Updated:
				ws = e.Updated.Workspace
			case *ccv1.WatchFleetResponse_Removed:
				ws = e.Removed.Workspace
			}
			if !allow(ws) {
				continue
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}
