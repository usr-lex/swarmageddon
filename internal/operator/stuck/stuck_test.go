package stuck

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func ev(name, obj, typ, reason, msg string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "p", Name: name},
		InvolvedObject: corev1.ObjectReference{Kind: "StatefulSet", Namespace: "p", Name: obj},
		Type:           typ, Reason: reason, Message: msg, LastTimestamp: metav1.NewTime(at),
	}
}

// The OpenShift report's Postgres: the StatefulSet's FailedCreate was the
// only sign it was never created.
func TestReason(t *testing.T) {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	now := time.Now()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		ev("a", "postgres", corev1.EventTypeWarning, "FailedCreate", "create Pod postgres-0 in StatefulSet postgres failed error: pods \"postgres-0\" is forbidden: unable to validate against any security context constraint", now.Add(-time.Minute)),
		ev("b", "postgres-0", corev1.EventTypeWarning, "FailedScheduling", "0/1 nodes are available: 1 Insufficient cpu.", now.Add(-2*time.Minute)),
		ev("c", "postgres", corev1.EventTypeNormal, "SuccessfulCreate", "ok", now),
		ev("d", "postgresql-other", corev1.EventTypeWarning, "FailedCreate", "someone else", now),
		ev("e", "forgejo", corev1.EventTypeWarning, "FailedCreate", "not ours", now),
	).Build()
	got, err := Reason(context.Background(), c, "p", "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if want := "FailedCreate: create Pod postgres-0 in StatefulSet postgres failed error: pods \"postgres-0\" is forbidden: unable to validate against any security context constraint"; got != want {
		t.Fatalf("got %q", got)
	}
	if got, _ := Reason(context.Background(), c, "p", "ntfy"); got != "" {
		t.Fatalf("no events: %q", got)
	}
}
