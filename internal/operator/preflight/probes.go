package preflight

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
)

// Probe object names.
const (
	podRWXWrite   = "rwx-write"
	podRWXRead    = "rwx-read"
	pvcRWX        = "rwx"
	podIdmap      = "idmap"
	pvcIdmap      = "idmap"
	podNPOpen     = "np-open"
	podNPClosed   = "np-closed"
	podNPClient   = "np-client"
	policyNPDeny  = "np-deny"
	podPodman     = "podman"
	podDNS        = "dns"
	labelProbe    = "swarmageddon.dev/probe"
	componentName = "preflight"
)

// probeState is what became of a probe pod.
type probeState int

const (
	statePending probeState = iota
	stateRunning
	stateSucceeded
	stateFailed
	stateImageUnavailable
	stateTimedOut
)

type probe struct {
	state    probeState
	exitCode int32
	message  string
	pod      *corev1.Pod
}

func (r *Runner) basePod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: Namespace,
			Labels: map[string]string{v1alpha1.LabelComponent: componentName, labelProbe: name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			EnableServiceLinks:            ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To[int64](1),
			ActiveDeadlineSeconds:         ptr.To(int64(r.timeout().Seconds())),
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: r.Images.Busybox,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		},
	}
}

func shellPod(r *Runner, name, script string) *corev1.Pod {
	p := r.basePod(name)
	p.Spec.Containers[0].Command = []string{"sh", "-c", script}
	return p
}

func withClaim(p *corev1.Pod, claim string) *corev1.Pod {
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
	}})
	p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data"})
	return p
}

// run creates the pod if missing (and create is set) and reports its state.
func (r *Runner) run(ctx context.Context, want *corev1.Pod, create bool) (probe, error) {
	var pod corev1.Pod
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), &pod)
	if apierrors.IsNotFound(err) {
		if !create {
			return probe{state: statePending}, nil
		}
		if err := r.Client.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
			return probe{}, fmt.Errorf("create probe %s: %w", want.Name, err)
		}
		return probe{state: statePending}, nil
	}
	if err != nil {
		return probe{}, err
	}
	if pod.DeletionTimestamp != nil {
		// Left over from an earlier run and still going away.
		return probe{state: statePending}, nil
	}
	return r.classify(&pod), nil
}

func (r *Runner) classify(pod *corev1.Pod) probe {
	p := probe{pod: pod}
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			p.exitCode = t.ExitCode
			p.message = strings.TrimSpace(t.Message)
			if p.message == "" {
				p.message = t.Reason
			}
		}
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
				p.state, p.message = stateImageUnavailable, fmt.Sprintf("image %s: %s %s", cs.Image, w.Reason, w.Message)
				return p
			case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
				p.state, p.message = stateFailed, w.Reason+": "+w.Message
				p.exitCode = -1
				return p
			}
		}
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		p.state = stateSucceeded
		return p
	case corev1.PodFailed:
		p.state = stateFailed
		if p.message == "" {
			p.message = pod.Status.Reason + " " + pod.Status.Message
		}
		if pod.Status.Reason == "DeadlineExceeded" {
			p.state = stateTimedOut
		}
		return p
	case corev1.PodRunning:
		p.state = stateRunning
	}
	if !pod.CreationTimestamp.IsZero() && r.now().Sub(pod.CreationTimestamp.Time) > r.timeout() {
		p.state = stateTimedOut
		p.message = fmt.Sprintf("still %s after %s", pod.Status.Phase, r.timeout())
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				p.message += ": " + c.Message
			}
		}
	}
	return p
}

func (r *Runner) ensureClaim(ctx context.Context, name string, mode corev1.PersistentVolumeAccessMode) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: Namespace, Labels: map[string]string{v1alpha1.LabelComponent: componentName}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{mode},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("16Mi")}},
		},
	}
	if err := r.Client.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func pending(name, msg string) v1alpha1.PreflightCheck {
	return v1alpha1.PreflightCheck{Name: name, Result: v1alpha1.CheckPending, Message: msg}
}

func probeErr(name string, err error) v1alpha1.PreflightCheck {
	return v1alpha1.PreflightCheck{Name: name, Result: v1alpha1.CheckWarn, Message: "probe error: " + err.Error()}
}

// volumeSharing: one pod writes to a ReadWriteMany claim, a second reads it.
func (r *Runner) volumeSharing(ctx context.Context) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckVolumeSharing}
	warn := func(msg string) v1alpha1.PreflightCheck {
		c.Result, c.Message = v1alpha1.CheckWarn, msg
		c.Fix = "Shared folders and the shared image store need a ReadWriteMany StorageClass on an idmap-capable filesystem (not NFS). Everything else works without it."
		return c
	}
	if err := r.ensureClaim(ctx, pvcRWX, corev1.ReadWriteMany); err != nil {
		return probeErr(c.Name, err)
	}
	w, err := r.run(ctx, withClaim(shellPod(r, podRWXWrite, "echo shared > /data/probe"), pvcRWX), true)
	if err != nil {
		return probeErr(c.Name, err)
	}
	switch w.state {
	case statePending, stateRunning:
		return pending(c.Name, "writer running")
	case stateSucceeded:
	default:
		return warn("no ReadWriteMany volumes: " + w.message)
	}
	rd, err := r.run(ctx, withClaim(shellPod(r, podRWXRead, "grep -qx shared /data/probe"), pvcRWX), true)
	if err != nil {
		return probeErr(c.Name, err)
	}
	switch rd.state {
	case statePending, stateRunning:
		return pending(c.Name, "reader running")
	case stateSucceeded:
		c.Result, c.Message = v1alpha1.CheckPass, "a ReadWriteMany claim is shared between pods"
		return c
	}
	return warn("the second pod could not read what the first wrote: " + rd.message)
}

// idmap: a user-namespaced pod writes to a fresh claim.
func (r *Runner) idmap(ctx context.Context) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckIdmap}
	if err := r.ensureClaim(ctx, pvcIdmap, corev1.ReadWriteOnce); err != nil {
		return probeErr(c.Name, err)
	}
	pod := withClaim(shellPod(r, podIdmap, "echo ok > /data/probe && chown 1000:1000 /data/probe && cat /data/probe"), pvcIdmap)
	pod.Spec.HostUsers = ptr.To(false)
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr.To[int64](0)}
	p, err := r.run(ctx, pod, true)
	if err != nil {
		return probeErr(c.Name, err)
	}
	switch p.state {
	case statePending, stateRunning:
		return pending(c.Name, "probe running")
	case stateSucceeded:
		c.Result, c.Message = v1alpha1.CheckPass, "a pod with hostUsers: false can write to a volume"
		return c
	case stateImageUnavailable:
		c.Result, c.Message = v1alpha1.CheckWarn, "probe image unavailable: "+p.message
		c.Fix = "Make " + r.Images.Busybox + " pullable (or override busybox with --images-from) and re-run preflight."
		return c
	}
	c.Result = v1alpha1.CheckFail
	c.Message = "a user-namespaced pod could not write to a volume: " + p.message
	c.Fix = "User namespaces need Linux 6.3+, containerd 2.0+ (or CRI-O) with runc 1.2+, and volumes on ext4, xfs or btrfs (not NFS). Without them the userns tier is off."
	return c
}

// networkPolicy: two servers, a deny policy on one; a client must reach
// the open one and not the closed one.
func (r *Runner) networkPolicy(ctx context.Context) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckNetworkPolicy}
	server := "echo ok > /tmp/index.html && exec httpd -f -p 8080 -h /tmp"
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: policyNPDeny, Namespace: Namespace, Labels: map[string]string{v1alpha1.LabelComponent: componentName}},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelProbe: podNPClosed}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if err := r.Client.Create(ctx, pol); err != nil && !apierrors.IsAlreadyExists(err) {
		return probeErr(c.Name, err)
	}
	var ips [2]string
	for i, name := range []string{podNPOpen, podNPClosed} {
		p, err := r.run(ctx, shellPod(r, name, server), true)
		if err != nil {
			return probeErr(c.Name, err)
		}
		switch {
		case p.state == stateRunning && p.pod.Status.PodIP != "":
			ips[i] = p.pod.Status.PodIP
		case p.state == statePending || p.state == stateRunning:
			return pending(c.Name, "starting probe servers")
		default:
			c.Result, c.Message = v1alpha1.CheckWarn, "probe server "+name+" did not run: "+p.message
			return c
		}
	}
	var current networkingv1.NetworkPolicy
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: Namespace, Name: policyNPDeny}, &current); err != nil {
		return probeErr(c.Name, err)
	}
	// Give the network plugin a moment to program the policy.
	if r.now().Sub(current.CreationTimestamp.Time).Seconds() < 5 {
		return pending(c.Name, "waiting for the policy to apply")
	}
	script := fmt.Sprintf(`get() { wget -q -T 3 -O /dev/null "http://$1:8080/"; }
ok=0; for i in 1 2 3 4 5; do if get %[1]s; then ok=1; break; fi; sleep 2; done
[ "$ok" = 1 ] || exit 2
for i in 1 2 3 4 5; do get %[2]s || exit 0; sleep 2; done
exit 1`, ips[0], ips[1])
	p, err := r.run(ctx, shellPod(r, podNPClient, script), true)
	if err != nil {
		return probeErr(c.Name, err)
	}
	switch p.state {
	case statePending, stateRunning:
		return pending(c.Name, "client probing")
	case stateSucceeded:
		c.Result, c.Message = v1alpha1.CheckPass, "a deny policy blocked traffic"
		return c
	case stateFailed:
		if p.exitCode == 1 {
			c.Result = v1alpha1.CheckFail
			c.Message = "NetworkPolicies are not enforced: rooms would not be isolated"
			c.Fix = "Use a network plugin that enforces NetworkPolicy (k3s does unless started with --disable-network-policy; or Calico, Cilium)."
			return c
		}
		if p.exitCode == 2 {
			c.Result, c.Message = v1alpha1.CheckWarn, "inconclusive: the client could not reach the open probe server either"
			return c
		}
	}
	c.Result, c.Message = v1alpha1.CheckWarn, "inconclusive: "+p.message
	return c
}

// RoomCapabilities are what a room's container adds in the userns tier. The
// podman probe asks for exactly these, so admission (an OpenShift SCC, a
// policy engine) that refuses a room refuses the probe too (OpenShift
// report 5.3: the probe passed without KILL and AUDIT_WRITE, rooms didn't).
var RoomCapabilities = []corev1.Capability{
	"SYS_ADMIN", "NET_ADMIN", "SETUID", "SETGID", "MKNOD", "CHOWN",
	"DAC_OVERRIDE", "FOWNER", "SYS_CHROOT", "SETFCAP", "KILL", "AUDIT_WRITE",
}

// podman: Podman in a user-namespaced pod, from the room image (ADR 0001).
func (r *Runner) podman(ctx context.Context) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckPodmanUserns}
	pod := r.basePod(podPodman)
	pod.Spec.HostUsers = ptr.To(false)
	ctr := &pod.Spec.Containers[0]
	ctr.Image = r.Images.Room
	ctr.Command = []string{"podman", "info", "--format", "{{.Host.OCIRuntime.Name}} {{.Store.GraphDriverName}}"}
	ctr.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}
	ctr.SecurityContext = &corev1.SecurityContext{
		RunAsUser:                ptr.To[int64](0),
		AllowPrivilegeEscalation: ptr.To(true),
		ProcMount:                ptr.To(corev1.UnmaskedProcMount),
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
		AppArmorProfile:          &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		Capabilities:             &corev1.Capabilities{Add: RoomCapabilities},
	}
	p, err := r.run(ctx, pod, true)
	if err != nil {
		return probeErr(c.Name, err)
	}
	switch p.state {
	case statePending, stateRunning:
		return pending(c.Name, "probe running")
	case stateSucceeded:
		c.Result, c.Message = v1alpha1.CheckPass, "podman info works in a user-namespaced pod"
		return c
	case stateImageUnavailable:
		c.Result = v1alpha1.CheckWarn
		c.Message = "unknown: the room image is not available (" + p.message + ")"
		c.Fix = "Make the room image pullable (or set --room-image) and re-run preflight to test Podman."
		return c
	case stateTimedOut:
		c.Result, c.Message = v1alpha1.CheckWarn, "unknown: the probe timed out ("+p.message+")"
		return c
	}
	c.Result = v1alpha1.CheckFail
	c.Message = "Podman did not run in a user-namespaced pod: " + p.message
	c.Fix = "The userns tier needs Kubernetes 1.33+, containerd 2.0+ with runc 1.2+ and Linux 6.3+. Otherwise Sysbox or (accepted) privileged rooms are used."
	return c
}

// dnsProbe resolves the kubernetes Service, retrying for a minute: on a new
// cluster preflight often starts before CoreDNS answers.
const dnsProbe = "for i in $(seq 30); do nslookup kubernetes.default.svc.cluster.local && exit 0; sleep 2; done; exit 1"

// dns: a probe pod resolves the kubernetes Service.
func (r *Runner) dns(ctx context.Context) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckDNS}
	p, err := r.run(ctx, shellPod(r, podDNS, dnsProbe), true)
	if err != nil {
		return probeErr(c.Name, err)
	}
	switch p.state {
	case statePending, stateRunning:
		return pending(c.Name, "probe running")
	case stateSucceeded:
		c.Result, c.Message = v1alpha1.CheckPass, "cluster DNS resolves Services"
		return c
	}
	c.Result = v1alpha1.CheckWarn
	c.Message = "cluster DNS lookup failed: " + p.message
	c.Fix = "Check that CoreDNS (kube-dns) is running and the cluster domain is cluster.local."
	return c
}

// Cleanup deletes the probe pods, claims and policy. The namespace stays.
func (r *Runner) Cleanup(ctx context.Context) error {
	sel := []client.DeleteAllOfOption{client.InNamespace(Namespace), client.MatchingLabels{v1alpha1.LabelComponent: componentName}}
	for _, obj := range []client.Object{&corev1.Pod{}, &corev1.PersistentVolumeClaim{}, &networkingv1.NetworkPolicy{}} {
		if err := r.Client.DeleteAllOf(ctx, obj, sel...); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
