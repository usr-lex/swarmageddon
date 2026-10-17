package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// The fleet tells the notifications engine each room pod's restarts and why
// its container last ended (roomCrashed, docs/design.md §10.3).
func TestFleetPodRestarts(t *testing.T) {
	at := func(m int) metav1.Time { return metav1.NewTime(time.Unix(int64(m)*60, 0)) }
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "room-athena-0", Namespace: "ws-acme", Labels: map[string]string{v1alpha1.LabelRoom: "athena"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "room", RestartCount: 2, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: at(5)}}},
			{Name: "sidecar", RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: at(2)}}},
		}}}
	e := newEnv(t, workspace("acme"), room("acme", "athena"), pod)
	var got []string
	e.srv.Fleet.OnPodRestarts = func(_ context.Context, ws, room string, n int32, reason string) {
		got = append(got, fmt.Sprintf("%s/%s %d %s", ws, room, n, reason))
	}
	if err := e.srv.Fleet.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "acme/athena 3 room: OOMKilled, exit code 137" {
		t.Fatalf("got %q", got)
	}
}
