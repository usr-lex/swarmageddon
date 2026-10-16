package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
)

// Annotations the room controller acts on, besides the ones in api/v1alpha1.
const (
	// AnnotationRecreateDeleteHome, with recreate, also deletes the home volume.
	AnnotationRecreateDeleteHome = "swarmageddon.dev/recreate-delete-home"
	// AnnotationDeleteDataNow, set before deleting a Room, skips volume retention.
	AnnotationDeleteDataNow = "swarmageddon.dev/delete-data-now"
)

type roomSvc struct{ *Server }

func (s roomSvc) ListRooms(ctx context.Context, req *connect.Request[ccv1.ListRoomsRequest]) (*connect.Response[ccv1.ListRoomsResponse], error) {
	p, err := signedIn(ctx)
	if err != nil {
		return nil, err
	}
	allowed := s.viewFilter(ctx, p)
	ws := req.Msg.GetWorkspace()
	rooms := s.Fleet.Rooms(func(w string) bool { return allowed(w) && (ws == "" || w == ws) })
	return connect.NewResponse(&ccv1.ListRoomsResponse{Rooms: rooms}), nil
}

func (s roomSvc) GetRoom(ctx context.Context, req *connect.Request[ccv1.GetRoomRequest]) (*connect.Response[ccv1.GetRoomResponse], error) {
	ref := req.Msg.GetRoom()
	if _, err := s.canView(ctx, ref.GetWorkspace()); err != nil {
		return nil, err
	}
	r, err := s.K.Room(ctx, ref.GetWorkspace(), ref.GetName())
	if err != nil {
		return nil, kerr(err)
	}
	spec, _ := json.Marshal(r.Spec)
	out := &ccv1.GetRoomResponse{Spec: string(spec), PodName: k8s.PodName(r.Name), ImageVersion: r.Status.ImageVersion,
		ForgeUser: r.Status.ForgeUser, CredentialsRotatedAt: tsMeta(r.Status.CredentialsRotatedAt)}
	for _, rs := range s.Fleet.Rooms(func(w string) bool { return w == ref.GetWorkspace() }) {
		if rs.Name == r.Name {
			out.Room = rs
		}
	}
	if pod, err := s.K.RoomPod(ctx, ref.GetWorkspace(), r.Name); err == nil {
		out.Node = pod.Spec.NodeName
	}
	if st := s.Fleet.Status(ref.GetWorkspace(), r.Name); st != nil {
		out.InputTokens, out.OutputTokens, out.ContextTokens, out.CliVersion = st.GetInputTokens(), st.GetOutputTokens(), st.GetContextTokens(), st.GetCliVersion()
	}
	return connect.NewResponse(out), nil
}

// lifecycle changes a Room as an operator of its workspace, audited.
func (s roomSvc) lifecycle(ctx context.Context, req connect.AnyRequest, ref *ccv1.RoomRef, action string, mutate func(r *v1alpha1.Room) error) error {
	if _, err := s.canOperate(ctx, ref.GetWorkspace()); err != nil {
		return err
	}
	var before, after any
	err := retryConflict(func() error {
		r, err := s.K.Room(ctx, ref.GetWorkspace(), ref.GetName())
		if err != nil {
			return err
		}
		before = r.Spec.DeepCopy()
		if err := mutate(r); err != nil {
			return err
		}
		after = r.Spec
		return s.K.C.Update(ctx, r)
	})
	s.audit(ctx, req, "room."+action, "room/"+ref.GetName(), ref.GetWorkspace(), before, after, err)
	s.Fleet.Kick()
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce
	}
	return kerr(err)
}

func annotate(r *v1alpha1.Room, kv ...string) {
	if r.Annotations == nil {
		r.Annotations = map[string]string{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Annotations[kv[i]] = kv[i+1]
	}
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (s roomSvc) PauseRoom(ctx context.Context, req *connect.Request[ccv1.PauseRoomRequest]) (*connect.Response[ccv1.PauseRoomResponse], error) {
	err := s.lifecycle(ctx, req, req.Msg.GetRoom(), "pause", func(r *v1alpha1.Room) error { r.Spec.Paused = true; return nil })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.PauseRoomResponse{}), nil
}

func (s roomSvc) ResumeRoom(ctx context.Context, req *connect.Request[ccv1.ResumeRoomRequest]) (*connect.Response[ccv1.ResumeRoomResponse], error) {
	err := s.lifecycle(ctx, req, req.Msg.GetRoom(), "resume", func(r *v1alpha1.Room) error { r.Spec.Paused = false; return nil })
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.ResumeRoomResponse{}), nil
}

func (s roomSvc) RecreateRoom(ctx context.Context, req *connect.Request[ccv1.RecreateRoomRequest]) (*connect.Response[ccv1.RecreateRoomResponse], error) {
	m := req.Msg
	if m.GetDeleteHome() && m.GetConfirmName() != m.GetRoom().GetName() {
		return nil, errInvalid("type the room's name to delete its home volume")
	}
	err := s.lifecycle(ctx, req, m.GetRoom(), "recreate", func(r *v1alpha1.Room) error {
		annotate(r, v1alpha1.AnnotationRecreate, now())
		if m.GetDeleteHome() {
			annotate(r, AnnotationRecreateDeleteHome, "true")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.RecreateRoomResponse{}), nil
}

func (s roomSvc) RemoveRoom(ctx context.Context, req *connect.Request[ccv1.RemoveRoomRequest]) (*connect.Response[ccv1.RemoveRoomResponse], error) {
	m := req.Msg
	ref := m.GetRoom()
	if m.GetConfirmName() != ref.GetName() {
		return nil, errInvalid("type the room's name to remove it")
	}
	if m.GetDeleteDataNow() {
		if err := s.lifecycle(ctx, req, ref, "markDeleteData", func(r *v1alpha1.Room) error {
			annotate(r, AnnotationDeleteDataNow, "true")
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if _, err := s.canOperate(ctx, ref.GetWorkspace()); err != nil {
		return nil, err
	}
	r := &v1alpha1.Room{}
	r.Name, r.Namespace = ref.GetName(), v1alpha1.WorkspaceNamespace(ref.GetWorkspace())
	err := s.K.C.Delete(ctx, r)
	s.audit(ctx, req, "room.remove", "room/"+ref.GetName(), ref.GetWorkspace(), nil, map[string]bool{"deleteDataNow": m.GetDeleteDataNow()}, err)
	if err != nil {
		return nil, kerr(err)
	}
	s.Fleet.Kick()
	return connect.NewResponse(&ccv1.RemoveRoomResponse{}), nil
}

func (s roomSvc) RotateRoomCredentials(ctx context.Context, req *connect.Request[ccv1.RotateRoomCredentialsRequest]) (*connect.Response[ccv1.RotateRoomCredentialsResponse], error) {
	err := s.lifecycle(ctx, req, req.Msg.GetRoom(), "rotate", func(r *v1alpha1.Room) error {
		annotate(r, v1alpha1.AnnotationRotate, now())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.RotateRoomCredentialsResponse{}), nil
}

func (s roomSvc) UpdateRoomNow(ctx context.Context, req *connect.Request[ccv1.UpdateRoomNowRequest]) (*connect.Response[ccv1.UpdateRoomNowResponse], error) {
	err := s.lifecycle(ctx, req, req.Msg.GetRoom(), "updateNow", func(r *v1alpha1.Room) error {
		annotate(r, v1alpha1.AnnotationUpdateNow, now())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.UpdateRoomNowResponse{}), nil
}

// AnnotationQuarantineReason carries why a room was quarantined.
const AnnotationQuarantineReason = "swarmageddon.dev/quarantine-reason"

func (s roomSvc) QuarantineRoom(ctx context.Context, req *connect.Request[ccv1.QuarantineRoomRequest]) (*connect.Response[ccv1.QuarantineRoomResponse], error) {
	err := s.lifecycle(ctx, req, req.Msg.GetRoom(), "quarantine", func(r *v1alpha1.Room) error {
		r.Spec.Quarantined = true
		reason := req.Msg.GetReason()
		if reason == "" {
			reason = "quarantined by " + displayName(authName(ctx), "a person")
		}
		annotate(r, AnnotationQuarantineReason, reason)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.QuarantineRoomResponse{}), nil
}

// ReleaseRoom lifts a quarantine. Only people with the operator role on the
// workspace may; the warden never releases.
func (s roomSvc) ReleaseRoom(ctx context.Context, req *connect.Request[ccv1.ReleaseRoomRequest]) (*connect.Response[ccv1.ReleaseRoomResponse], error) {
	err := s.lifecycle(ctx, req, req.Msg.GetRoom(), "release", func(r *v1alpha1.Room) error {
		if !r.Spec.Quarantined {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("the room is not quarantined"))
		}
		r.Spec.Quarantined = false
		annotate(r, "swarmageddon.dev/released-by", authName(ctx), "swarmageddon.dev/release-note", req.Msg.GetNote())
		delete(r.Annotations, AnnotationQuarantineReason)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ccv1.ReleaseRoomResponse{}), nil
}

func authName(ctx context.Context) string { return auth.FromContext(ctx).Name() }

// quarantineReason reads why a room is quarantined: its Quarantined
// condition, else the annotation.
func quarantineReason(r *v1alpha1.Room) string {
	for _, c := range r.Status.Conditions {
		if c.Type == v1alpha1.ConditionQuarantined && c.Status == "True" && c.Message != "" {
			return c.Message
		}
	}
	return r.Annotations[AnnotationQuarantineReason]
}

func (s roomSvc) StreamRoomLogs(ctx context.Context, req *connect.Request[ccv1.StreamRoomLogsRequest], stream *connect.ServerStream[ccv1.StreamRoomLogsResponse]) error {
	m := req.Msg
	ref := m.GetRoom()
	if _, err := s.canView(ctx, ref.GetWorkspace()); err != nil {
		return err
	}
	opts := &corev1.PodLogOptions{Container: "room", Follow: m.GetFollow(), Previous: m.GetPrevious()}
	tail := m.GetTailLines()
	if tail <= 0 && m.GetSinceSeconds() <= 0 {
		tail = 500
	}
	if tail > 0 {
		opts.TailLines = &tail
	}
	if ss := m.GetSinceSeconds(); ss > 0 {
		opts.SinceSeconds = &ss
	}
	rc, err := s.K.Kube.CoreV1().Pods(v1alpha1.WorkspaceNamespace(ref.GetWorkspace())).GetLogs(k8s.PodName(ref.GetName()), opts).Stream(ctx)
	if err != nil {
		return kerr(err)
	}
	defer rc.Close()
	comp := m.GetComponent()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if comp != "" && !matchesComponent(line, comp) {
			continue
		}
		if err := stream.Send(&ccv1.StreamRoomLogsResponse{Line: line}); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// matchesComponent filters supervisor log lines by component: the
// supervisor logs JSON with a "component" field, or key=value text.
func matchesComponent(line, comp string) bool {
	return strings.Contains(line, `"component":"`+comp+`"`) || strings.Contains(line, "component="+comp)
}

func (s roomSvc) GetRoomResources(ctx context.Context, req *connect.Request[ccv1.GetRoomResourcesRequest]) (*connect.Response[ccv1.GetRoomResourcesResponse], error) {
	ref := req.Msg.GetRoom()
	if _, err := s.canView(ctx, ref.GetWorkspace()); err != nil {
		return nil, err
	}
	out := &ccv1.GetRoomResourcesResponse{}
	pod, err := s.K.RoomPod(ctx, ref.GetWorkspace(), ref.GetName())
	if err != nil {
		return nil, kerr(err)
	}
	for _, c := range pod.Spec.Containers {
		if l := c.Resources.Limits; l != nil {
			out.CpuLimitMillicores += l.Cpu().MilliValue()
			out.MemoryLimitBytes += l.Memory().Value()
		}
	}
	m, err := s.K.PodMetrics(ctx, pod.Namespace, pod.Name)
	if err != nil {
		return nil, kerr(err)
	}
	if m != nil {
		out.MetricsAvailable = true
		out.At = ts(m.Timestamp.Time)
		for _, c := range m.Containers {
			out.CpuMillicores += c.Usage.Cpu().MilliValue()
			out.MemoryBytes += c.Usage.Memory().Value()
		}
	}
	return connect.NewResponse(out), nil
}

func (s roomSvc) GetRoomStorage(ctx context.Context, req *connect.Request[ccv1.GetRoomStorageRequest]) (*connect.Response[ccv1.GetRoomStorageResponse], error) {
	ref := req.Msg.GetRoom()
	if _, err := s.canView(ctx, ref.GetWorkspace()); err != nil {
		return nil, err
	}
	r, err := s.K.Room(ctx, ref.GetWorkspace(), ref.GetName())
	if err != nil {
		return nil, kerr(err)
	}
	out := &ccv1.GetRoomStorageResponse{}
	if st := s.Fleet.Status(ref.GetWorkspace(), ref.GetName()); st != nil {
		for _, v := range st.GetVolumes() {
			out.Volumes = append(out.Volumes, &ccv1.VolumeUsage{Name: v.GetName(), UsedBytes: v.GetUsedBytes(), CapacityBytes: v.GetCapacityBytes()})
		}
	}
	claims, err := s.K.RoomClaims(ctx, ref.GetWorkspace(), ref.GetName())
	if err != nil {
		return nil, kerr(err)
	}
	for _, c := range claims {
		ci := &ccv1.ClaimInfo{Name: c.Name, Phase: string(c.Status.Phase)}
		if c.Spec.StorageClassName != nil {
			ci.StorageClass = *c.Spec.StorageClassName
		}
		if q, ok := c.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			ci.Requested = q.String()
		}
		if q, ok := c.Status.Capacity[corev1.ResourceStorage]; ok {
			ci.Capacity = q.String()
		}
		out.Claims = append(out.Claims, ci)
	}
	for _, d := range r.Spec.SharedDirs { //nolint:staticcheck // still shown for rooms that use the deprecated field
		out.SharedDirs = append(out.SharedDirs, d.Name)
	}
	return connect.NewResponse(out), nil
}

var _ = types.NamespacedName{}
var _ client.Object
