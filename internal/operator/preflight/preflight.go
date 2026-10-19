// Package preflight checks whether the cluster can run swarmageddon
// (docs/design.md §3.4) and picks the rooms' isolation tier (ADR 0001).
//
// Checks that need a running workload use small, time-bounded probe pods in
// the namespace swarmageddon-preflight. Run is non-blocking: each call
// creates the probes that are missing, reads the ones that finished and
// returns the checks so far, so a controller calls it on every reconcile
// until it reports done.
package preflight

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
)

// Namespace holds the probe pods.
const Namespace = "swarmageddon-preflight"

// Check names, in the order they are reported.
const (
	CheckKubernetesVersion = "kubernetesVersion"
	CheckArchitecture      = "architecture"
	CheckStorageClass      = "defaultStorageClass"
	CheckVolumeSharing     = "volumeSharing"
	CheckSizeEnforcement   = "volumeSizeEnforcement"
	CheckIdmap             = "idmapVolumes"
	CheckNetworkPolicy     = "networkPolicy"
	CheckPodmanUserns      = "podmanInUserNamespace"
	CheckSysbox            = "sysboxRuntimeClass"
	CheckPrivileged        = "privilegedAdmission"
	CheckTier              = "isolationTier"
	CheckCapacity          = "capacity"
	CheckIngress           = "ingress"
	CheckDNS               = "clusterDNS"
	CheckClusterCIDRs      = "clusterCIDRs"
	CheckSecretsEncryption = "secretsEncryption"
)

// Default room size the estimate uses (design §4 example).
var (
	RoomCPUMillis   int64 = 2000
	RoomMemoryBytes int64 = 6 << 30
)

// Runner runs the checks.
type Runner struct {
	Client client.Client
	// ServerVersion is the discovery client's ServerVersion.
	ServerVersion func() (*version.Info, error)
	Images        images.Images
	// Now defaults to time.Now.
	Now func() time.Time
	// ProbeTimeout bounds each probe pod; default 3 minutes.
	ProbeTimeout time.Duration
	// OpenShift: the probes' namespace gets SCC swarmageddon-rooms, as rooms do.
	OpenShift bool
}

// Result of a whole run.
type Result struct {
	Status *v1alpha1.PreflightStatus
	// Done is true when no check is pending.
	Done bool
}

// Run evaluates every check once.
func (r *Runner) Run(ctx context.Context, spec *v1alpha1.PlatformSpec) (Result, error) {
	if err := r.ensureNamespace(ctx); err != nil {
		return Result{}, err
	}
	cluster, err := r.readCluster(ctx)
	if err != nil {
		return Result{}, err
	}
	st := &v1alpha1.PreflightStatus{}
	add := func(c v1alpha1.PreflightCheck) { st.Checks = append(st.Checks, c) }

	minor, verCheck := r.kubernetesVersion()
	add(verCheck)
	add(architecture(cluster.nodes))
	sc, scCheck := defaultStorageClass(cluster.storageClasses, cluster.isK3s)
	add(scCheck)
	if sc != nil {
		add(r.volumeSharing(ctx))
		add(sizeEnforcement(sc.Provisioner))
		idmap := r.idmap(ctx)
		add(idmap)
	} else {
		add(skipped(CheckVolumeSharing, "no default StorageClass"))
		add(skipped(CheckSizeEnforcement, "no default StorageClass"))
		add(v1alpha1.PreflightCheck{Name: CheckIdmap, Result: v1alpha1.CheckFail, Message: "no default StorageClass to test with", Fix: "Install a StorageClass first (see defaultStorageClass)."})
	}
	add(r.networkPolicy(ctx))
	podman := r.podman(ctx)
	add(podman)
	add(sysboxCheck(cluster.sysbox))
	priv, err := r.privilegedAdmitted(ctx)
	if err != nil {
		return Result{}, err
	}
	add(priv)
	tier, tierCheck := ChooseTier(spec, minor, find(st.Checks, CheckIdmap), podman, cluster.sysbox, priv.Result == v1alpha1.CheckPass)
	add(tierCheck)
	st.Tier = tier
	est, capCheck := capacity(cluster.nodes, cluster.pods)
	st.RoomEstimate = est
	add(capCheck)
	add(ingressCheck(cluster.ingressClasses, cluster.openshift))
	add(r.dns(ctx))
	cidrs, cidrCheck := clusterCIDRs(spec, cluster)
	st.ClusterCIDRs = cidrs
	add(cidrCheck)
	add(secretsEncryption(cluster.isK3s))

	done := true
	for _, c := range st.Checks {
		if c.Result == v1alpha1.CheckPending {
			done = false
		}
	}
	return Result{Status: st, Done: done}, nil
}

func find(checks []v1alpha1.PreflightCheck, name string) v1alpha1.PreflightCheck {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return v1alpha1.PreflightCheck{Name: name, Result: v1alpha1.CheckPending}
}

func skipped(name, why string) v1alpha1.PreflightCheck {
	return v1alpha1.PreflightCheck{Name: name, Result: v1alpha1.CheckWarn, Message: "skipped: " + why}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) timeout() time.Duration {
	if r.ProbeTimeout > 0 {
		return r.ProbeTimeout
	}
	return 3 * time.Minute
}

// kubernetesVersion returns the minor version (0 if unknown) and the check.
func (r *Runner) kubernetesVersion() (int, v1alpha1.PreflightCheck) {
	c := v1alpha1.PreflightCheck{Name: CheckKubernetesVersion}
	if r.ServerVersion == nil {
		c.Result, c.Message = v1alpha1.CheckWarn, "server version unknown"
		return 0, c
	}
	info, err := r.ServerVersion()
	if err != nil {
		c.Result, c.Message = v1alpha1.CheckWarn, "reading the server version: "+err.Error()
		return 0, c
	}
	major, _ := strconv.Atoi(strings.TrimRight(info.Major, "+"))
	minor, _ := strconv.Atoi(strings.TrimRight(info.Minor, "+"))
	return minor, EvaluateVersion(major, minor, info.GitVersion)
}

// EvaluateVersion: < 1.33 fails, 1.33 to 1.35 warns (user namespaces beta), 1.36+ passes.
func EvaluateVersion(major, minor int, git string) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckKubernetesVersion, Message: "Kubernetes " + git}
	switch {
	case major != 1 || minor < 33:
		c.Result = v1alpha1.CheckFail
		c.Fix = "Upgrade to Kubernetes 1.33 or later; 1.36 or later is recommended."
	case minor < 36:
		c.Result = v1alpha1.CheckWarn
		c.Message += ": user namespaces are beta in this version"
		c.Fix = "Upgrade to Kubernetes 1.36 or later, where user namespaces are GA."
	default:
		c.Result = v1alpha1.CheckPass
	}
	return c
}

func architecture(nodes []corev1.Node) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckArchitecture}
	arches := map[string]int{}
	var bad []string
	for _, n := range nodes {
		a := n.Labels[corev1.LabelArchStable]
		arches[a]++
		if a != "amd64" && a != "arm64" {
			bad = append(bad, fmt.Sprintf("%s (%s)", n.Name, a))
		}
	}
	var parts []string
	for a, n := range arches {
		parts = append(parts, fmt.Sprintf("%d %s", n, a))
	}
	c.Message = "nodes: " + strings.Join(sortedStrings(parts), ", ")
	switch {
	case len(nodes) == 0:
		c.Result, c.Message = v1alpha1.CheckFail, "no nodes"
	case len(bad) == len(nodes):
		c.Result, c.Fix = v1alpha1.CheckFail, "Rooms run on amd64 and arm64 nodes only."
	case len(bad) > 0:
		c.Result = v1alpha1.CheckWarn
		c.Message += "; rooms won't be scheduled on " + strings.Join(bad, ", ")
	default:
		c.Result = v1alpha1.CheckPass
	}
	return c
}

func sysboxCheck(found bool) v1alpha1.PreflightCheck {
	if found {
		return v1alpha1.PreflightCheck{Name: CheckSysbox, Result: v1alpha1.CheckPass, Message: "RuntimeClass sysbox-runc exists"}
	}
	return v1alpha1.PreflightCheck{Name: CheckSysbox, Result: v1alpha1.CheckPass, Message: "no sysbox-runc RuntimeClass (optional)"}
}

func ingressCheck(classes []ingressClass, openshift bool) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{Name: CheckIngress}
	for _, ic := range classes {
		if ic.isDefault {
			c.Result, c.Message = v1alpha1.CheckPass, "default IngressClass "+ic.name
			return c
		}
	}
	c.Result = v1alpha1.CheckWarn
	c.Message = "no default IngressClass: the control center is exposed on a NodePort"
	c.Fix = "Install an ingress controller and mark its IngressClass default (k3s ships Traefik), or use kubectl port-forward svc/control-center 8443:443 -n swarmageddon-system."
	if openshift {
		c.Fix = "On OpenShift, expose it through the router with a passthrough Route: oc -n swarmageddon-system create route passthrough control-center --service control-center"
	}
	return c
}

func secretsEncryption(k3s bool) v1alpha1.PreflightCheck {
	c := v1alpha1.PreflightCheck{
		Name:    CheckSecretsEncryption,
		Result:  v1alpha1.CheckWarn,
		Message: "whether the API server encrypts Secrets at rest can't be read through the API",
		Fix:     "Enable encryption at rest for Secrets (an EncryptionConfiguration on the API server).",
	}
	if k3s {
		c.Fix = "On k3s, start the server with --secrets-encryption (or run k3s secrets-encrypt enable), then check with k3s secrets-encrypt status."
	}
	return c
}

func sortedStrings(s []string) []string {
	slices.Sort(s)
	return s
}
