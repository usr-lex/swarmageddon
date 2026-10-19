// Package stuck says why a workload the operator owns isn't coming up, from
// the Warning events Kubernetes records: an admission refusal (FailedCreate)
// or a pod that can't be placed (FailedScheduling). Without it the wizard
// showed "Deploying" and rooms "waiting for the pod" forever while the only
// sign was an event (OpenShift report, issue 5.4).
package stuck

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reasons are the events that explain a workload not starting.
var reasons = map[string]bool{
	"FailedCreate":               true, // StatefulSet, ReplicaSet, Job: admission refused the pod
	"FailedScheduling":           true, // no node fits (capacity, taints, unbound volumes)
	"FailedAttachVolume":         true,
	"FailedMount":                true,
	"CreateContainerConfigError": true,
}

// Reason is the newest explaining Warning event about the workload name in
// ns, or about its ReplicaSets and pods (name-...), as "Reason: message";
// "" when there is none. r should read around the cache (events aren't
// watched).
func Reason(ctx context.Context, r client.Reader, ns, name string) (string, error) {
	var list corev1.EventList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return "", err
	}
	var best *corev1.Event
	for i := range list.Items {
		e := &list.Items[i]
		n := e.InvolvedObject.Name
		if e.Type != corev1.EventTypeWarning || !reasons[e.Reason] || (n != name && !strings.HasPrefix(n, name+"-")) {
			continue
		}
		if best == nil || last(e).After(last(best)) {
			best = e
		}
	}
	if best == nil {
		return "", nil
	}
	msg := strings.Join(strings.Fields(best.Message), " ")
	if len(msg) > 400 {
		msg = msg[:397] + "..."
	}
	return best.Reason + ": " + msg, nil
}

// last is when the event was last seen.
func last(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}
