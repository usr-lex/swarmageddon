package preflight

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/openshift"
)

type ingressClass struct {
	name      string
	isDefault bool
}

// cluster is what the read-only checks look at.
type cluster struct {
	nodes          []corev1.Node
	pods           []corev1.Pod
	storageClasses []storagev1.StorageClass
	ingressClasses []ingressClass
	sysbox         bool
	isK3s          bool
	serviceCIDRs   []string
	kubernetesIP   string
	apiEndpoints   []string
}

func (r *Runner) readCluster(ctx context.Context) (*cluster, error) {
	c := &cluster{}
	var nodes corev1.NodeList
	if err := r.Client.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	c.nodes = nodes.Items
	for _, n := range nodes.Items {
		if strings.Contains(n.Status.NodeInfo.KubeletVersion, "k3s") {
			c.isK3s = true
		}
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	c.pods = pods.Items
	var scs storagev1.StorageClassList
	if err := r.Client.List(ctx, &scs); err != nil {
		return nil, fmt.Errorf("list storage classes: %w", err)
	}
	c.storageClasses = scs.Items
	var ics networkingv1.IngressClassList
	if err := r.Client.List(ctx, &ics); err != nil {
		return nil, fmt.Errorf("list ingress classes: %w", err)
	}
	for _, ic := range ics.Items {
		c.ingressClasses = append(c.ingressClasses, ingressClass{name: ic.Name, isDefault: ic.Annotations[networkingv1.AnnotationIsDefaultIngressClass] == "true"})
	}
	var rc nodev1.RuntimeClass
	err := r.Client.Get(ctx, types.NamespacedName{Name: "sysbox-runc"}, &rc)
	switch {
	case err == nil:
		c.sysbox = true
	case !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err):
		return nil, fmt.Errorf("get runtimeclass: %w", err)
	}
	var cidrs networkingv1.ServiceCIDRList
	if err := r.Client.List(ctx, &cidrs); err == nil {
		for _, s := range cidrs.Items {
			c.serviceCIDRs = append(c.serviceCIDRs, s.Spec.CIDRs...)
		}
	}
	var svc corev1.Service
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, &svc); err == nil {
		c.kubernetesIP = svc.Spec.ClusterIP
	}
	var slices discoveryv1.EndpointSliceList
	if err := r.Client.List(ctx, &slices, client.InNamespace("default"), client.MatchingLabels{discoveryv1.LabelServiceName: "kubernetes"}); err == nil {
		for _, s := range slices.Items {
			for _, e := range s.Endpoints {
				c.apiEndpoints = append(c.apiEndpoints, e.Addresses...)
			}
		}
	}
	return c, nil
}

// defaultStorageClass returns the default class, if any.
func defaultStorageClass(classes []storagev1.StorageClass, k3s bool) (*storagev1.StorageClass, v1alpha1.PreflightCheck) {
	c := v1alpha1.PreflightCheck{Name: CheckStorageClass}
	for i, sc := range classes {
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			c.Result, c.Message = v1alpha1.CheckPass, fmt.Sprintf("%s (%s)", sc.Name, sc.Provisioner)
			return &classes[i], c
		}
	}
	c.Result = v1alpha1.CheckFail
	c.Message = "no default StorageClass"
	c.Fix = "Install one, for example local-path: kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v0.0.37/deploy/local-path-storage.yaml && kubectl patch storageclass local-path -p '{\"metadata\":{\"annotations\":{\"storageclass.kubernetes.io/is-default-class\":\"true\"}}}'"
	if k3s {
		c.Fix = "k3s ships local-path; it is missing only if the server runs with --disable=local-storage. Remove that flag and restart k3s."
	}
	return nil, c
}

var enforcingProvisioners = []string{
	"topolvm.io", "topolvm.cybozu.com", "local.csi.openebs.io", "zfs.csi.openebs.io", "driver.longhorn.io",
	"ebs.csi.aws.com", "pd.csi.storage.gke.io", "disk.csi.azure.com", "cinder.csi.openstack.org",
	"csi.hetzner.cloud", "dobs.csi.digitalocean.com", "rbd.csi.ceph.com", "kubernetes.io/aws-ebs",
	"kubernetes.io/gce-pd", "kubernetes.io/azure-disk",
}

func sizeEnforcement(provisioner string) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckSizeEnforcement}
	switch {
	case slices.Contains(enforcingProvisioners, provisioner):
		c.Result, c.Message = v1alpha1.CheckPass, provisioner+" enforces volume sizes"
	case provisioner == "rancher.io/local-path":
		c.Result = v1alpha1.CheckWarn
		c.Message = "local-path does not enforce volume sizes: room quotas are soft (the janitor and warnings enforce them)"
		c.Fix = "For hard limits use TopoLVM or OpenEBS LVM, or local-path on an XFS mount with project quotas."
	default:
		c.Result = v1alpha1.CheckWarn
		c.Message = provisioner + ": unknown whether it enforces volume sizes; treating quotas as soft"
	}
	return c
}

// capacity estimates how many default-size rooms fit in free allocatable
// CPU and memory.
func capacity(nodes []corev1.Node, pods []corev1.Pod) (int32, v1alpha1.PreflightCheck) {
	type used struct{ cpu, mem int64 }
	usage := map[string]*used{}
	for _, p := range pods {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		u := usage[p.Spec.NodeName]
		if u == nil {
			u = &used{}
			usage[p.Spec.NodeName] = u
		}
		for _, ctr := range p.Spec.Containers {
			u.cpu += ctr.Resources.Requests.Cpu().MilliValue()
			u.mem += ctr.Resources.Requests.Memory().Value()
		}
	}
	var rooms, freeCPU, freeMem, freeDisk int64
	for _, n := range nodes {
		if n.Spec.Unschedulable {
			continue
		}
		u := usage[n.Name]
		if u == nil {
			u = &used{}
		}
		cpu := n.Status.Allocatable.Cpu().MilliValue() - u.cpu
		mem := n.Status.Allocatable.Memory().Value() - u.mem
		freeCPU += max(cpu, 0)
		freeMem += max(mem, 0)
		freeDisk += n.Status.Allocatable.StorageEphemeral().Value()
		rooms += max(0, min(cpu/RoomCPUMillis, mem/RoomMemoryBytes))
	}
	c := v1alpha1.PreflightCheck{
		Name: CheckCapacity,
		Message: fmt.Sprintf("free: %s CPU, %s memory; node ephemeral storage %s; about %d rooms at %s CPU / %s",
			resource.NewMilliQuantity(freeCPU, resource.DecimalSI), resource.NewQuantity(freeMem, resource.BinarySI).String(),
			resource.NewQuantity(freeDisk, resource.BinarySI).String(), rooms,
			resource.NewMilliQuantity(RoomCPUMillis, resource.DecimalSI), resource.NewQuantity(RoomMemoryBytes, resource.BinarySI)),
		Result: v1alpha1.CheckPass,
	}
	if rooms < 1 {
		c.Result = v1alpha1.CheckWarn
		c.Fix = "Add nodes or capacity, or give rooms less CPU and memory in the workspace wizard."
	}
	return int32(rooms), c
}

// clusterCIDRs returns the ranges rooms may not reach: pod CIDRs, service
// CIDRs, node addresses and API server endpoints.
func clusterCIDRs(spec *v1alpha1.PlatformSpec, c *cluster) ([]string, v1alpha1.PreflightCheck) {
	check := v1alpha1.PreflightCheck{Name: CheckClusterCIDRs}
	if len(spec.Egress.ClusterCIDRs) > 0 {
		check.Result, check.Message = v1alpha1.CheckPass, "set in spec.egress.clusterCIDRs"
		return spec.Egress.ClusterCIDRs, check
	}
	var out []string
	addHost := func(ip string) {
		if p := net.ParseIP(ip); p != nil {
			if p.To4() != nil {
				out = append(out, ip+"/32")
			} else {
				out = append(out, ip+"/128")
			}
		}
	}
	podCIDRs := 0
	for _, n := range c.nodes {
		for _, p := range n.Spec.PodCIDRs {
			out = append(out, p)
			podCIDRs++
		}
		if len(n.Spec.PodCIDRs) == 0 && n.Spec.PodCIDR != "" {
			out = append(out, n.Spec.PodCIDR)
			podCIDRs++
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				addHost(a.Address)
			}
		}
	}
	out = append(out, c.serviceCIDRs...)
	if c.kubernetesIP != "" && len(c.serviceCIDRs) == 0 {
		addHost(c.kubernetesIP)
	}
	for _, e := range c.apiEndpoints {
		addHost(e)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	var missing []string
	if podCIDRs == 0 {
		missing = append(missing, "pod CIDR (nodes have no podCIDR; common with Calico or Cilium IPAM)")
	}
	if len(c.serviceCIDRs) == 0 {
		missing = append(missing, "service CIDR (no ServiceCIDR objects)")
	}
	check.Message = strings.Join(out, ", ")
	if len(missing) == 0 {
		check.Result = v1alpha1.CheckPass
		return out, check
	}
	check.Result = v1alpha1.CheckWarn
	check.Message = "detected " + check.Message + "; could not detect " + strings.Join(missing, " and ") +
		". Private ranges (RFC 1918, CGNAT, link-local) are denied to rooms anyway"
	check.Fix = "If your pod or service ranges are outside private address space, list them in spec.egress.clusterCIDRs."
	return out, check
}

// privilegedAdmitted asks admission, with a dry run, whether a privileged
// pod would be accepted in a namespace labelled like a privileged-tier workspace.
func (r *Runner) privilegedAdmitted(ctx context.Context) (v1alpha1.PreflightCheck, error) {
	c := v1alpha1.PreflightCheck{Name: CheckPrivileged}
	pod := r.basePod("privileged-dryrun")
	pod.Spec.Containers[0].Command = []string{"true"}
	priv := true
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &priv}
	err := r.Client.Create(ctx, pod, client.DryRunAll)
	switch {
	case err == nil:
		c.Result, c.Message = v1alpha1.CheckPass, "privileged pods are admitted"
	case apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		c.Result, c.Message = v1alpha1.CheckWarn, "privileged pods are refused: "+err.Error()
		c.Fix = "Only needed for the privileged tier; allow privileged pods in ws-* namespaces if you choose it."
	case apierrors.IsAlreadyExists(err):
		c.Result, c.Message = v1alpha1.CheckPass, "privileged pods are admitted"
	default:
		return c, err
	}
	return c, nil
}

func (r *Runner) ensureNamespace(ctx context.Context) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: Namespace, Labels: map[string]string{
		v1alpha1.LabelManaged:                "true",
		v1alpha1.LabelComponent:              "preflight",
		"pod-security.kubernetes.io/enforce": "privileged",
	}}}
	err := r.Client.Create(ctx, ns)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	if r.OpenShift {
		err := r.Client.Create(ctx, openshift.Binding(Namespace, openshift.Rooms))
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// ChooseTier picks the isolation tier (ADR 0001) from the probes.
func ChooseTier(spec *v1alpha1.PlatformSpec, minor int, idmap, podman v1alpha1.PreflightCheck, sysbox, privAdmitted bool) (v1alpha1.IsolationTier, v1alpha1.PreflightCheck) {
	c := v1alpha1.PreflightCheck{Name: CheckTier}
	privileged := func(why string) (v1alpha1.IsolationTier, v1alpha1.PreflightCheck) {
		if !privAdmitted {
			c.Result = v1alpha1.CheckFail
			c.Message = why + ", and privileged pods are refused: no tier can run rooms"
			c.Fix = "Use Kubernetes 1.33+ with a 6.3+ kernel and containerd 2 for user namespaces, or install Sysbox."
			return "", c
		}
		if !spec.PrivilegedAccepted {
			c.Result = v1alpha1.CheckFail
			c.Message = why + ": only the privileged tier is left, and isolation does not hold in it"
			c.Fix = "Accept the privileged tier in the wizard (spec.privilegedAccepted), or fix the user-namespace checks above."
			return v1alpha1.TierPrivileged, c
		}
		c.Result = v1alpha1.CheckWarn
		c.Message = why + ": rooms run privileged; isolation does not hold"
		return v1alpha1.TierPrivileged, c
	}
	if spec.IsolationTier != "" && spec.IsolationTier != v1alpha1.TierAuto {
		if spec.IsolationTier == v1alpha1.TierPrivileged {
			return privileged("tier set to privileged")
		}
		c.Result, c.Message = v1alpha1.CheckPass, "set to "+string(spec.IsolationTier)+" in spec"
		if spec.IsolationTier == v1alpha1.TierSysbox && !sysbox {
			c.Result, c.Message = v1alpha1.CheckFail, "spec asks for sysbox but there is no sysbox-runc RuntimeClass"
		}
		return spec.IsolationTier, c
	}
	if podman.Result == v1alpha1.CheckPending || idmap.Result == v1alpha1.CheckPending {
		c.Result, c.Message = v1alpha1.CheckPending, "waiting for probes"
		return "", c
	}
	if podman.Result == v1alpha1.CheckPass {
		c.Result, c.Message = v1alpha1.CheckPass, "userns: Podman runs in a user-namespaced pod"
		return v1alpha1.TierUserns, c
	}
	podmanUnknown := podman.Result == v1alpha1.CheckWarn
	if podmanUnknown && idmap.Result == v1alpha1.CheckPass && minor >= 33 {
		c.Result = v1alpha1.CheckWarn
		c.Message = "userns: user-namespaced pods with volumes work, but Podman inside one was not tested (" + podman.Message + ")"
		return v1alpha1.TierUserns, c
	}
	if sysbox {
		c.Result, c.Message = v1alpha1.CheckPass, "sysbox: the sysbox-runc RuntimeClass exists"
		return v1alpha1.TierSysbox, c
	}
	return privileged("user namespaces can't run rooms here and there is no Sysbox")
}
