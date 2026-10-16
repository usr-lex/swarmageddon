// Package runnerpool is the RunnerPool controller (docs/design.md §9.4): it
// polls Forgejo for waiting jobs with the pool's labels, registers one
// ephemeral runner per job, runs `forgejo-runner one-job` in a pod with
// capped emptyDir storage, and deletes the pod when the job is done.
package runnerpool

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/room"
)

// Labels, annotations and conditions.
const (
	ComponentRunner = "runner"
	LabelPool       = "swarmageddon.dev/runner-pool"
	AnnJobID        = "swarmageddon.dev/job-id"
	AnnRunnerID     = "swarmageddon.dev/runner-id"

	ConditionReady   = "Ready"
	ConditionDiskLow = "DiskLow"

	pollInterval = 10 * time.Second
	tokenPath    = "/run/runner/token"
)

var (
	defaultCPU     = resource.MustParse("2")
	defaultMemory  = resource.MustParse("4Gi")
	defaultStorage = resource.MustParse("40Gi")
)

// FreeDiskFunc returns the most free ephemeral storage on any schedulable
// node, in bytes; ok is false when it can't tell.
type FreeDiskFunc func(ctx context.Context) (free int64, ok bool, err error)

// Reconciler reconciles RunnerPools.
type Reconciler struct {
	client.Client
	Deps deps.Deps
	// FreeDisk measures node disk; nil uses node allocatable minus requests.
	FreeDisk FreeDiskFunc
}

// +kubebuilder:rbac:groups=swarmageddon.dev,resources=runnerpools;runnerpools/status,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=swarmageddon.dev,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager registers the RunnerPool controller.
func SetupWithManager(mgr ctrl.Manager, d deps.Deps) error {
	r := &Reconciler{Client: mgr.GetClient(), Deps: d}
	return ctrl.NewControllerManagedBy(mgr).
		Named("runnerpool").
		For(&v1.RunnerPool{}).
		Owns(&corev1.Pod{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

// Reconcile runs one polling round for a pool.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool v1.RunnerPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // pods are owned and garbage-collected
	}
	st := pool.Status.DeepCopy()
	st.ObservedGeneration = pool.Generation
	err := r.round(ctx, &pool, st)
	if err != nil {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse,
			Reason: "Error", Message: err.Error(), ObservedGeneration: pool.Generation})
	}
	if fmt.Sprint(stripTimes(st)) != fmt.Sprint(stripTimes(&pool.Status)) {
		pool.Status = *st
		if uerr := r.Status().Update(ctx, &pool); uerr != nil {
			return ctrl.Result{}, uerr
		}
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "runner pool round")
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

func stripTimes(s *v1.RunnerPoolStatus) v1.RunnerPoolStatus {
	c := *s.DeepCopy()
	for i := range c.Conditions {
		c.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	return c
}

func (r *Reconciler) round(ctx context.Context, pool *v1.RunnerPool, st *v1.RunnerPoolStatus) error {
	wsName := room.WorkspaceOf(pool.Namespace)
	var ws v1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: wsName}, &ws); err != nil {
		return fmt.Errorf("workspace %q: %w", wsName, err)
	}
	org := ws.Spec.Forge.Org
	if err := r.ensureNetworkPolicy(ctx, pool, &ws); err != nil {
		return err
	}
	if r.Deps.Forge == nil {
		return fmt.Errorf("no forge client")
	}
	fc, err := r.Deps.Forge(ctx)
	if err != nil {
		return fmt.Errorf("forge: %w", err)
	}

	// Collect finished pods; count the rest.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(pool.Namespace), client.MatchingLabels{LabelPool: pool.Name, v1.LabelComponent: ComponentRunner}); err != nil {
		return err
	}
	active := map[string]bool{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			if id, err := strconv.ParseInt(p.Annotations[AnnRunnerID], 10, 64); err == nil {
				_ = fc.DeleteOrgRunner(ctx, org, id) // ephemeral runners are removed by Forgejo; this catches the rest
			}
			if err := r.Delete(ctx, p, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			continue
		}
		if p.DeletionTimestamp.IsZero() {
			active[p.Annotations[AnnJobID]] = true
		}
	}
	st.Running = int32(len(active))

	jobs, err := fc.WaitingJobs(ctx, org, pool.Spec.Labels)
	if err != nil {
		return fmt.Errorf("waiting jobs: %w", err)
	}
	repoIDs, err := r.repoIDs(ctx, fc, org, pool.Spec.Repos)
	if err != nil {
		return err
	}
	var todo []forgejo.RunJob
	for _, j := range jobs {
		if active[strconv.FormatInt(j.ID, 10)] || !servable(j, pool.Spec.Labels, repoIDs) {
			continue
		}
		todo = append(todo, j)
	}
	st.Waiting = int32(len(todo))

	maxC := pool.Spec.MaxConcurrent
	if maxC <= 0 {
		maxC = 4
	}
	if len(todo) > 0 && int32(len(active)) < maxC {
		low, msg := r.diskLow(ctx, pool)
		cond := metav1.Condition{Type: ConditionDiskLow, Status: metav1.ConditionFalse, Reason: "Enough", Message: msg, ObservedGeneration: pool.Generation}
		if low {
			cond.Status, cond.Reason = metav1.ConditionTrue, "BelowMinimum"
		}
		meta.SetStatusCondition(&st.Conditions, cond)
		if low {
			meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Polling", Message: "jobs wait: node disk is low", ObservedGeneration: pool.Generation})
			return nil
		}
	}
	for _, j := range todo {
		if int32(len(active)) >= maxC {
			break
		}
		if err := r.startJob(ctx, fc, pool, &ws, j); err != nil {
			return err
		}
		active[strconv.FormatInt(j.ID, 10)] = true
		st.Waiting--
	}
	st.Running = int32(len(active))
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue,
		Reason: "Polling", ObservedGeneration: pool.Generation})
	return nil
}

// servable: the runner gets exactly the pool's labels, so it can take a job
// only if every runs-on label is one of them.
func servable(j forgejo.RunJob, labels []string, repoIDs map[int64]bool) bool {
	if len(j.RunsOn) == 0 {
		return false
	}
	for _, l := range j.RunsOn {
		if !slices.Contains(labels, l) {
			return false
		}
	}
	return repoIDs == nil || repoIDs[j.RepoID]
}

func (r *Reconciler) repoIDs(ctx context.Context, fc *forgejo.Client, org string, repos []string) (map[int64]bool, error) {
	if len(repos) == 0 {
		return nil, nil
	}
	ids := map[int64]bool{}
	for _, full := range repos {
		owner, name := org, full
		if o, n, ok := cutSlash(full); ok {
			owner, name = o, n
		}
		rp, err := fc.GetRepo(ctx, owner, name)
		if forgejo.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ids[rp.ID] = true
	}
	return ids, nil
}

func cutSlash(s string) (string, string, bool) {
	for i := range len(s) {
		if s[i] == '/' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// PodName is the pod of a pool's job.
func PodName(pool string, jobID int64) string { return fmt.Sprintf("runner-%s-%d", pool, jobID) }

func (r *Reconciler) startJob(ctx context.Context, fc *forgejo.Client, pool *v1.RunnerPool, ws *v1.Workspace, j forgejo.RunJob) error {
	name := PodName(pool.Name, j.ID)
	reg, err := fc.RegisterOrgRunner(ctx, ws.Spec.Forge.Org, name, "swarmageddon runner pool "+pool.Namespace+"/"+pool.Name, true)
	if err != nil {
		return fmt.Errorf("register runner: %w", err)
	}
	image := pool.Spec.Image
	if image == "" && r.Deps.Images != nil {
		image = r.Deps.Images.Runner()
	}
	tier := string(v1.TierUserns)
	if r.Deps.PlatformTier != nil {
		if t, err := r.Deps.PlatformTier(ctx); err == nil && t != "" && t != string(v1.TierAuto) {
			tier = t
		}
	}
	pod := desiredPod(pool, ws.Name, name, image, tier, reg, j)
	if err := controllerutil.SetControllerReference(pool, pod, r.Scheme()); err != nil {
		return err
	}
	if err := r.Create(ctx, pod); err != nil {
		_ = fc.DeleteOrgRunner(ctx, ws.Spec.Forge.Org, reg.ID)
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pool.Namespace, Labels: pod.Labels},
		StringData: map[string]string{"token": reg.Token},
	}
	if err := controllerutil.SetControllerReference(pod, sec, r.Scheme()); err != nil {
		return err
	}
	return r.Create(ctx, sec)
}

func desiredPod(pool *v1.RunnerPool, ws, name, image, tier string, reg *forgejo.RegisteredRunner, j forgejo.RunJob) *corev1.Pod {
	cpu, mem, disk := defaultCPU, defaultMemory, defaultStorage
	if q := pool.Spec.Resources.CPU; q != nil {
		cpu = *q
	}
	if q := pool.Spec.Resources.Memory; q != nil {
		mem = *q
	}
	if q := pool.Spec.Resources.EphemeralStorage; q != nil {
		disk = *q
	}
	args := []string{"one-job", "--url", room.ForgeURL, "--uuid", reg.UUID, "--token-url", "file:" + tokenPath}
	if j.Handle != "" {
		args = append(args, "--handle", j.Handle)
	}
	for _, l := range pool.Spec.Labels {
		args = append(args, "--label", l+":host") // the pod itself is the job's isolation
	}
	labels := map[string]string{
		v1.LabelComponent: ComponentRunner, LabelPool: pool.Name, v1.LabelWorkspace: ws, v1.LabelManaged: "true",
	}
	c := corev1.Container{
		Name:    "runner",
		Image:   image,
		Command: []string{"forgejo-runner"},
		Args:    args,
		Env:     []corev1.EnvVar{{Name: "DOCKER_HOST", Value: "unix:///var/run/docker.sock"}},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem, corev1.ResourceEphemeralStorage: disk},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem, corev1.ResourceEphemeralStorage: disk},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "token", MountPath: "/run/runner", ReadOnly: true},
			{Name: "work", MountPath: "/var/lib/containers", SubPath: "engine"},
			{Name: "work", MountPath: "/data", SubPath: "data"},
		},
	}
	spec := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		AutomountServiceAccountToken:  ptr.To(false),
		EnableServiceLinks:            ptr.To(false),
		ActiveDeadlineSeconds:         ptr.To[int64](6 * 3600),
		TerminationGracePeriodSeconds: ptr.To[int64](30),
		Volumes: []corev1.Volume{
			{Name: "token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: name, DefaultMode: ptr.To[int32](0o400)}}},
			{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &disk}}},
		},
	}
	room.ApplyTier(&spec, &c, tier)
	spec.Containers = []corev1.Container{c}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pool.Namespace, Labels: labels, Annotations: map[string]string{
			AnnJobID: strconv.FormatInt(j.ID, 10), AnnRunnerID: strconv.FormatInt(reg.ID, 10),
		}},
		Spec: spec,
	}
}

// RunnerPlatformPeers are what runner pods reach: Forgejo and the registry
// cache, never rooms (§9.4).
var RunnerPlatformPeers = []room.PlatformPeer{
	{App: "forgejo", Ports: []int32{3000, 22, 2222}},
	{App: "registry-cache", Ports: []int32{5000}},
}

func (r *Reconciler) ensureNetworkPolicy(ctx context.Context, pool *v1.RunnerPool, ws *v1.Workspace) error {
	// CI jobs keep the internet unless the workspace turned it off the old
	// way; room network modes (§11.2) don't apply to runners.
	internet := ws.Spec.Internet == nil || *ws.Spec.Internet //nolint:staticcheck // deprecated field still honored
	e := room.Egress{Internet: internet, Allowed: ws.Spec.AllowedCIDRs}
	var p v1.Platform
	if err := r.Get(ctx, types.NamespacedName{Name: v1.PlatformName}, &p); err == nil {
		cl := p.Spec.Egress.ClusterCIDRs
		if len(cl) == 0 && p.Status.Preflight != nil {
			cl = p.Status.Preflight.ClusterCIDRs
		}
		e.Denied = append(append(e.Denied, cl...), p.Spec.Egress.ExtraDeniedCIDRs...)
	}
	want := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{LabelPool: pool.Name, v1.LabelComponent: ComponentRunner}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Egress:      room.EgressRules(RunnerPlatformPeers, e),
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "runners-" + pool.Name, Namespace: pool.Namespace}}
	err := r.Get(ctx, client.ObjectKeyFromObject(np), np)
	if apierrors.IsNotFound(err) {
		np.Spec = want
		np.Labels = map[string]string{LabelPool: pool.Name, v1.LabelManaged: "true"}
		if err := controllerutil.SetControllerReference(pool, np, r.Scheme()); err != nil {
			return err
		}
		return r.Create(ctx, np)
	}
	if err != nil {
		return err
	}
	if np.Spec.String() == want.String() {
		return nil
	}
	np.Spec = want
	return r.Update(ctx, np)
}

// diskLow reports whether no node has minFreeDiskGB free.
func (r *Reconciler) diskLow(ctx context.Context, pool *v1.RunnerPool) (bool, string) {
	minGB := int64(pool.Spec.MinFreeDiskGB)
	if minGB <= 0 {
		return false, ""
	}
	f := r.FreeDisk
	if f == nil {
		f = r.allocatableFree
	}
	free, ok, err := f(ctx)
	if err != nil || !ok {
		return false, "node disk unknown"
	}
	msg := fmt.Sprintf("most free on a node: %d GB, minimum %d GB", free>>30, minGB)
	return free < minGB<<30, msg
}

// allocatableFree is the fallback measure: per schedulable node, allocatable
// ephemeral storage minus what running pods request; the best node counts.
func (r *Reconciler) allocatableFree(ctx context.Context) (int64, bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return 0, false, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return 0, false, err
	}
	used := map[string]int64{}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			if q, ok := c.Resources.Requests[corev1.ResourceEphemeralStorage]; ok {
				used[p.Spec.NodeName] += q.Value()
			}
		}
	}
	best, ok := int64(0), false
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable {
			continue
		}
		q, has := n.Status.Allocatable[corev1.ResourceEphemeralStorage]
		if !has {
			continue
		}
		ok = true
		if free := q.Value() - used[n.Name]; free > best {
			best = free
		}
	}
	return best, ok, nil
}
