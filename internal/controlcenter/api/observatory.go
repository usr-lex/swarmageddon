package api

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/oklog/ulid/v2"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

type observatorySvc struct{ *Server }

func (s observatorySvc) open(ctx context.Context, ws string) (*store.Store, error) {
	if _, err := s.canView(ctx, ws); err != nil {
		return nil, err
	}
	db := s.DB()
	if db == nil {
		return nil, errNoDB()
	}
	return db, nil
}

func edgeKind(k string) (ccv1.EdgeKind, string) {
	switch k {
	case store.KindMessage:
		return ccv1.EdgeKind_EDGE_KIND_MESSAGE, ""
	case store.KindCrossing:
		return ccv1.EdgeKind_EDGE_KIND_CROSSING, ""
	case store.KindHuman:
		return ccv1.EdgeKind_EDGE_KIND_HUMAN, ""
	case store.KindPost:
		return ccv1.EdgeKind_EDGE_KIND_CHANNEL_POST, ""
	}
	return ccv1.EdgeKind_EDGE_KIND_FORGE_HANDOFF, k
}

func (s observatorySvc) Edges(ctx context.Context, req *connect.Request[ccv1.EdgesRequest]) (*connect.Response[ccv1.EdgesResponse], error) {
	db, err := s.open(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	until := time.Now()
	if req.Msg.GetUntil() != nil {
		until = req.Msg.GetUntil().AsTime()
	}
	since := until.Add(-24 * time.Hour)
	if req.Msg.GetSince() != nil {
		since = req.Msg.GetSince().AsTime()
	}
	edges, err := db.Edges(ctx, req.Msg.GetWorkspace(), since, until)
	if err != nil {
		return nil, err
	}
	out := &ccv1.EdgesResponse{}
	for _, e := range edges {
		k, sub := edgeKind(e.Kind)
		out.Edges = append(out.Edges, &ccv1.Edge{From: e.From, To: e.To, Kind: k, Subkind: sub, Count: e.Count})
	}
	return connect.NewResponse(out), nil
}

var presenceByName = map[string]ccv1.Presence{
	"working": ccv1.Presence_PRESENCE_WORKING, "waitingOnHuman": ccv1.Presence_PRESENCE_WAITING_ON_HUMAN,
	"idle": ccv1.Presence_PRESENCE_IDLE, "offline": ccv1.Presence_PRESENCE_OFFLINE,
}

func (s observatorySvc) Now(ctx context.Context, req *connect.Request[ccv1.NowRequest]) (*connect.Response[ccv1.NowResponse], error) {
	ws := req.Msg.GetWorkspace()
	db, err := s.open(ctx, ws)
	if err != nil {
		return nil, err
	}
	rows, err := db.Now(ctx, ws)
	if err != nil {
		return nil, err
	}
	byName := map[string]*ccv1.AgentNow{}
	for _, r := range rows {
		a := &ccv1.AgentNow{Name: r.Actor, Presence: presenceByName[r.Presence], PresenceSince: ts(r.PresenceSince), CurrentTask: r.Task}
		if r.ItemNumber > 0 {
			a.LastItem = &ccv1.ItemRef{Repo: r.ItemRepo, Number: int32(r.ItemNumber), Title: r.ItemTitle, Url: r.ItemURL, At: ts(r.ItemAt)}
		}
		byName[r.Actor] = a
	}
	// Live state wins over the table, and only rooms are agents.
	var out []*ccv1.AgentNow
	for _, rs := range s.Fleet.Rooms(func(w string) bool { return w == ws }) {
		a := byName[rs.Name]
		if a == nil {
			a = &ccv1.AgentNow{Name: rs.Name}
		}
		a.Role = rs.Role
		if rs.Presence != ccv1.Presence_PRESENCE_UNSPECIFIED {
			a.Presence = rs.Presence
			if rs.PresenceSince != nil {
				a.PresenceSince = rs.PresenceSince
			}
		}
		if rs.CurrentTask != "" {
			a.CurrentTask = rs.CurrentTask
		}
		out = append(out, a)
	}
	order := map[ccv1.Presence]int{ccv1.Presence_PRESENCE_WAITING_ON_HUMAN: 0, ccv1.Presence_PRESENCE_WORKING: 1, ccv1.Presence_PRESENCE_IDLE: 2}
	rank := func(p ccv1.Presence) int {
		if r, ok := order[p]; ok {
			return r
		}
		return 3
	}
	sort.SliceStable(out, func(i, j int) bool {
		if rank(out[i].Presence) != rank(out[j].Presence) {
			return rank(out[i].Presence) < rank(out[j].Presence)
		}
		return out[i].Name < out[j].Name
	})
	return connect.NewResponse(&ccv1.NowResponse{Agents: out}), nil
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func (s observatorySvc) Heatmap(ctx context.Context, req *connect.Request[ccv1.HeatmapRequest]) (*connect.Response[ccv1.HeatmapResponse], error) {
	db, err := s.open(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	days := req.Msg.GetDays()
	if days <= 0 || days > 3660 {
		days = 365
	}
	cells, err := db.Heatmap(ctx, req.Msg.GetWorkspace(), req.Msg.GetActor(), time.Now().AddDate(0, 0, -int(days)))
	if err != nil {
		return nil, err
	}
	out := &ccv1.HeatmapResponse{}
	for _, c := range cells {
		out.Cells = append(out.Cells, &ccv1.HeatmapCell{Actor: c.Actor, Day: day(c.Day), Count: c.Count})
	}
	return connect.NewResponse(out), nil
}

func (s observatorySvc) Axes(ctx context.Context, req *connect.Request[ccv1.AxesRequest]) (*connect.Response[ccv1.AxesResponse], error) {
	db, err := s.open(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	since := time.Now().AddDate(0, 0, -30)
	if req.Msg.GetSince() != nil {
		since = req.Msg.GetSince().AsTime()
	}
	rows, err := db.Axes(ctx, req.Msg.GetWorkspace(), since)
	if err != nil {
		return nil, err
	}
	out := &ccv1.AxesResponse{}
	for _, r := range rows {
		out.Rows = append(out.Rows, &ccv1.AxesRow{Actor: r.Actor, Commits: r.Commits, PullRequests: r.PullRequests, Reviews: r.Reviews, Issues: r.Issues, Messages: r.Messages})
	}
	return connect.NewResponse(out), nil
}

func activityProto(a store.Activity) *ccv1.ActivityEvent {
	return &ccv1.ActivityEvent{Id: a.ID, At: ts(a.TS), Actor: a.Actor, Kind: a.Kind, Target: a.Target, Repo: a.Repo,
		Number: int32(a.Number), Summary: a.Summary, Url: a.RefURL, Value: a.Value}
}

func (s observatorySvc) Timeline(ctx context.Context, req *connect.Request[ccv1.TimelineRequest]) (*connect.Response[ccv1.TimelineResponse], error) {
	m := req.Msg
	db, err := s.open(ctx, m.GetWorkspace())
	if err != nil {
		return nil, err
	}
	limit := int(m.GetLimit())
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	evs, err := db.Timeline(ctx, m.GetWorkspace(), store.TimelineFilter{Actors: m.GetActors(), Kinds: m.GetKinds(), Repo: m.GetRepo(),
		BeforeID: m.GetBeforeId(), Limit: limit})
	if err != nil {
		return nil, err
	}
	out := &ccv1.TimelineResponse{}
	for _, e := range evs {
		out.Events = append(out.Events, activityProto(e))
	}
	if len(evs) == limit {
		out.NextBeforeId = evs[len(evs)-1].ID
	}
	return connect.NewResponse(out), nil
}

func (s observatorySvc) WatchTimeline(ctx context.Context, req *connect.Request[ccv1.WatchTimelineRequest], stream *connect.ServerStream[ccv1.WatchTimelineResponse]) error {
	if _, err := s.open(ctx, req.Msg.GetWorkspace()); err != nil {
		return err
	}
	ch, cancel := s.Rec.Subscribe(req.Msg.GetWorkspace())
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(&ccv1.WatchTimelineResponse{Event: activityProto(e)}); err != nil {
				return err
			}
		}
	}
}

func (s observatorySvc) Throughput(ctx context.Context, req *connect.Request[ccv1.ThroughputRequest]) (*connect.Response[ccv1.ThroughputResponse], error) {
	ws := req.Msg.GetWorkspace()
	db, err := s.open(ctx, ws)
	if err != nil {
		return nil, err
	}
	days := req.Msg.GetDays()
	if days <= 0 || days > 366 {
		days = 30
	}
	tp, lat, err := db.Throughput(ctx, ws, time.Now().AddDate(0, 0, -int(days)))
	if err != nil {
		return nil, err
	}
	out := &ccv1.ThroughputResponse{}
	for _, r := range tp {
		out.Days = append(out.Days, &ccv1.ThroughputDay{Day: day(r.Day), Actor: r.Actor, Merged: r.Merged, CiFailures: r.CIFailures, Tokens: r.Tokens})
	}
	for _, l := range lat {
		out.ReviewLatency = append(out.ReviewLatency, &ccv1.ReviewLatency{Day: day(l.Day), MedianSeconds: l.MedianSeconds, Reviews: l.Reviews})
	}
	for _, k := range s.Spend.Latest().Rooms {
		if k.Workspace == ws {
			out.Spend = append(out.Spend, &ccv1.RoomSpend{Workspace: ws, Room: k.Room, SpendUsd: k.Spend, BudgetUsd: k.MaxBudget})
		}
	}
	return connect.NewResponse(out), nil
}

func (s observatorySvc) Reports(ctx context.Context, req *connect.Request[ccv1.ReportsRequest]) (*connect.Response[ccv1.ReportsResponse], error) {
	db, err := s.open(ctx, req.Msg.GetWorkspace())
	if err != nil {
		return nil, err
	}
	reps, err := db.Reports(ctx, req.Msg.GetWorkspace(), int(req.Msg.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := &ccv1.ReportsResponse{}
	for _, r := range reps {
		out.Reports = append(out.Reports, &ccv1.Report{Id: r.ID, Sender: r.Sender, At: ts(r.TS), Body: r.Body})
	}
	return connect.NewResponse(out), nil
}

// RequestReport sends the chronicler(s) a message from the person.
func (s observatorySvc) RequestReport(ctx context.Context, req *connect.Request[ccv1.RequestReportRequest]) (*connect.Response[ccv1.RequestReportResponse], error) {
	ws := req.Msg.GetWorkspace()
	p, err := s.canOperate(ctx, ws)
	if err != nil {
		return nil, err
	}
	if s.Msg == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("messaging is not connected"))
	}
	rooms, err := s.K.Rooms(ctx, ws)
	if err != nil {
		return nil, kerr(err)
	}
	body := "Please send a status report to status@" + ws + " now."
	if n := strings.TrimSpace(req.Msg.GetNote()); n != "" {
		body += "\n\n" + n
	}
	out := &ccv1.RequestReportResponse{}
	for _, r := range rooms {
		if r.Spec.Role != "chronicler" {
			continue
		}
		to := r.Name + "@" + ws
		_, err := s.Msg.SendAs(ctx, p.Handle+"@"+ws, ulid.Make().String(), to, body, "")
		s.audit(ctx, req, "observatory.requestReport", "room/"+r.Name, ws, nil, nil, err)
		if err != nil {
			return nil, err
		}
		out.SentTo = append(out.SentTo, to)
	}
	if len(out.SentTo) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this workspace has no chronicler"))
	}
	return connect.NewResponse(out), nil
}
