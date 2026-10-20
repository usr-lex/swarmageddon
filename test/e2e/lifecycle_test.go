//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
)

// identity is what must not change in a room when another one changes: its
// pod and its StatefulSet's generation (design §5).
type identity struct {
	pod types.UID
	gen int64
}

func (s *suite) identities(t *testing.T) map[string]identity {
	t.Helper()
	ctx := context.Background()
	out := map[string]identity{}
	for _, r := range planned {
		var p corev1.Pod
		var sts appsv1.StatefulSet
		if err := s.kube.Get(ctx, types.NamespacedName{Namespace: wsNS, Name: podOf(r)}, &p); err != nil {
			t.Fatal(err)
		}
		if err := s.kube.Get(ctx, types.NamespacedName{Namespace: wsNS, Name: "room-" + r}, &sts); err != nil {
			t.Fatal(err)
		}
		out[r] = identity{p.UID, sts.Generation}
	}
	return out
}

// addRemoveRoom adds a room with the wizard, removes it from the room's page
// (RemoveRoom, deleting its data now), and checks no other room was touched.
func (s *suite) addRemoveRoom(t *testing.T) {
	before := s.identities(t)
	s.apply(t, plan(agent("iris", "reviewer", "claude", "clean")), false)
	s.waitRooms(t, "iris")
	if _, err := call(s.cc.rooms.RemoveRoom, &ccv1.RemoveRoomRequest{Room: ref("iris"), DeleteDataNow: true, ConfirmName: "iris"}); err != nil {
		t.Fatalf("RemoveRoom(iris): %v", err)
	}
	s.eventually(t, 5*time.Minute, "iris gone from the fleet and its pod deleted", func() error {
		lr, err := call(s.cc.rooms.ListRooms, &ccv1.ListRoomsRequest{Workspace: ws})
		if err != nil {
			return err
		}
		if slices.ContainsFunc(lr.GetRooms(), func(r *ccv1.RoomSummary) bool { return r.GetName() == "iris" }) {
			return errors.New("iris still listed")
		}
		err = s.kube.Get(context.Background(), types.NamespacedName{Namespace: wsNS, Name: podOf("iris")}, &corev1.Pod{})
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("pod: %v", err)
		}
		return nil
	})
	after := s.identities(t)
	for r, b := range before {
		if a := after[r]; a != b {
			t.Errorf("%s changed while iris came and went: pod %s -> %s, StatefulSet generation %d -> %d", r, b.pod, a.pod, b.gen, a.gen)
		}
	}
	if pv, err := call(s.cc.workspaces.PreviewWorkspace, &ccv1.PreviewWorkspaceRequest{Plan: plan()}); err == nil {
		if c := change(pv.GetChanges(), "Room", "iris"); c != nil {
			t.Errorf("the wizard still plans a change for the removed room: %v", c)
		}
	}
}

// gatewayStatus asks the gateway for a model reply with the room's own key,
// from inside the room, and returns the HTTP status. api is messages
// (Anthropic, as Claude Code calls it) or responses (OpenAI, as ore does).
func (s *suite) gatewayStatus(room, api, model string) (string, error) {
	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`
	if api == "responses" {
		body = `{"model":"` + model + `","input":"ping"}`
	}
	out, err := s.inRoomAsRoot(room, `k=$(cat /run/swarm/identity/gateway_key); curl -s -o /dev/null -w '%{http_code}' --max-time 60 -X POST `+
		`http://gateway.swarmageddon-platform.svc:4000/v1/`+api+` -H "x-api-key: $k" -H "Authorization: Bearer $k" `+
		`-H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' -d '`+body+`'`)
	return strings.TrimSpace(out), err
}

// modelReplies: each CLI's model API answers through the gateway (warden,
// LiteLLM, the mock model) with a room's own key (§10.1, §14).
func (s *suite) modelReplies(t *testing.T) {
	for _, c := range []struct{ room, api, model string }{
		{"athena", "messages", "mock-claude"},
		{"hermes", "responses", "mock-ore"},
	} {
		if code, err := s.gatewayStatus(c.room, c.api, c.model); err != nil || code != "200" {
			t.Errorf("%s: POST /v1/%s (%s) through the gateway: %v: HTTP %s, want 200", c.room, c.api, c.model, err, code)
		}
	}
}

// passed says the warden let a request through to LiteLLM: anything but its
// refusals (401 unknown key, 403 quarantined) or no answer.
func passed(code string) bool { return code != "" && code != "000" && code != "401" && code != "403" }

// quarantine quarantines athena from the control center and checks the warden
// refuses her model requests with 403 until she is released (§6.5).
func (s *suite) quarantine(t *testing.T) {
	if code, err := s.gatewayStatus("athena", "messages", "mock-claude"); err != nil || !passed(code) {
		t.Errorf("athena's gateway request before the quarantine: %v: HTTP %s, want it passed on", err, code)
	}
	if _, err := call(s.cc.rooms.QuarantineRoom, &ccv1.QuarantineRoomRequest{Room: ref("athena"), Reason: "e2e " + s.nonce}); err != nil {
		t.Fatalf("QuarantineRoom: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_, _ = call(s.cc.rooms.ReleaseRoom, &ccv1.ReleaseRoomRequest{Room: ref("athena"), Note: "e2e cleanup"})
		}
	}()
	s.eventually(t, 3*time.Minute, "the fleet shows athena quarantined", func() error {
		gr, err := call(s.cc.rooms.GetRoom, &ccv1.GetRoomRequest{Room: ref("athena")})
		if err != nil {
			return err
		}
		if !gr.GetRoom().GetQuarantined() {
			return errors.New("not quarantined")
		}
		return nil
	})
	s.eventually(t, 3*time.Minute, "the warden refuses athena with 403", func() error {
		code, err := s.gatewayStatus("athena", "messages", "mock-claude")
		if err != nil || code != "403" {
			return fmt.Errorf("%v: HTTP %s", err, code)
		}
		return nil
	})
	if code, err := s.gatewayStatus("artemis", "messages", "mock-claude"); err != nil || !passed(code) {
		t.Errorf("artemis (not quarantined) while athena is: %v: HTTP %s, want it passed on", err, code)
	}
	if _, err := call(s.cc.rooms.ReleaseRoom, &ccv1.ReleaseRoomRequest{Room: ref("athena"), Note: "e2e: checked"}); err != nil {
		t.Fatalf("ReleaseRoom: %v", err)
	}
	released = true
	s.eventually(t, 3*time.Minute, "athena's gateway requests pass again", func() error {
		code, err := s.gatewayStatus("athena", "messages", "mock-claude")
		if err != nil || !passed(code) {
			return fmt.Errorf("%v: HTTP %s", err, code)
		}
		return nil
	})
}

// observatory reads the who-talks-to-whom edges for the messages the
// messaging steps sent (§8.7).
func (s *suite) observatory(t *testing.T) {
	want := []struct {
		from, to string
		kind     ccv1.EdgeKind
	}{
		{"athena", "hermes", ccv1.EdgeKind_EDGE_KIND_MESSAGE},
		{"hermes", "metis", ccv1.EdgeKind_EDGE_KIND_CROSSING},
		{admin, "athena", ccv1.EdgeKind_EDGE_KIND_HUMAN},
	}
	defer func() {
		if !t.Failed() {
			return
		}
		// What the audit follower stored, oldest first: where it stops
		// and what comes before the gap.
		res, err := call(s.cc.observatory.Timeline, &ccv1.TimelineRequest{Workspace: ws, Limit: 200,
			Kinds: []string{"message", "human", "crossing", "post", "refusal", "verdict"}})
		if err != nil {
			t.Logf("timeline: %v", err)
			return
		}
		ev := res.GetEvents()
		for i := len(ev) - 1; i >= 0; i-- {
			e := ev[i]
			t.Logf("timeline %s %-8s %s -> %s %s", e.GetAt().AsTime().Format(time.RFC3339Nano), e.GetKind(), e.GetActor(), e.GetTarget(), clip(e.GetSummary()))
		}
	}()
	s.eventually(t, 3*time.Minute, "observatory edges", func() error {
		res, err := call(s.cc.observatory.Edges, &ccv1.EdgesRequest{Workspace: ws, Since: timestamppb.New(s.start.Add(-time.Minute))})
		if err != nil {
			return err
		}
		var missing []string
		for _, w := range want {
			if !slices.ContainsFunc(res.GetEdges(), func(e *ccv1.Edge) bool {
				return e.GetFrom() == w.from && e.GetTo() == w.to && e.GetKind() == w.kind && e.GetCount() > 0
			}) {
				missing = append(missing, fmt.Sprintf("%s->%s %v", w.from, w.to, w.kind))
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("missing %v in %v", missing, res.GetEdges())
		}
		return nil
	})
}
