package room

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"slices"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	v1 "github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/devices"
	"github.com/usr-lex/swarmageddon/internal/operator/clusterdns"
	"github.com/usr-lex/swarmageddon/internal/operator/preflight"
)

// Platform endpoints (docs/conventions.md).
const (
	ForgeURL     = "http://forgejo." + v1.PlatformNamespace + ".svc:3000"
	ForgeSSHHost = "forgejo-ssh." + v1.PlatformNamespace + ".svc"
	MessagingURL = "https://messaging." + v1.PlatformNamespace + ".svc:8443"
	GatewayURL   = "http://gateway." + v1.PlatformNamespace + ".svc:4000"

	// CABundleConfigMap is the platform CA bundle in every managed namespace.
	CABundleConfigMap = "platform-ca-bundle"
	// LabelAppName selects platform service pods in NetworkPolicies.
	LabelAppName = "app.kubernetes.io/name"
	// ComponentRoom labels room objects.
	ComponentRoom = "room"

	// AnnotationTemplateHash on the StatefulSet records the pod template it was rendered from.
	AnnotationTemplateHash = "swarmageddon.dev/template-hash"
	// AnnotationPersonaHash on the pod template rolls the pod when its persona changes.
	AnnotationPersonaHash = "swarmageddon.dev/persona-hash"

	vncPort = 5900
)

// Default sizes (docs/design.md §4).
var (
	defaultCPU    = resource.MustParse("2")
	defaultMemory = resource.MustParse("6Gi")
	defaultHome   = resource.MustParse("30Gi")
	defaultEngine = resource.MustParse("60Gi")
)

// Name of a room's objects.
func Name(room string) string { return "room-" + room }

// IdentitySecretName is the room's identity Secret.
func IdentitySecretName(room string) string { return Name(room) + "-identity" }

// PersonaConfigMapName is the room's persona ConfigMap.
func PersonaConfigMapName(room string) string { return Name(room) + "-persona" }

// WorkspaceOf returns the workspace a namespace belongs to ("" if none).
func WorkspaceOf(namespace string) string {
	if !strings.HasPrefix(namespace, v1.WorkspaceNamespacePrefix) {
		return ""
	}
	return strings.TrimPrefix(namespace, v1.WorkspaceNamespacePrefix)
}

// SelectorLabels select a room's pod.
func SelectorLabels(room string) map[string]string {
	return map[string]string{v1.LabelRoom: room, v1.LabelComponent: ComponentRoom}
}

func objectLabels(r *v1.Room, ws string) map[string]string {
	l := SelectorLabels(r.Name)
	l[v1.LabelWorkspace] = ws
	l[v1.LabelManaged] = "true"
	return l
}

// ApplyTier sets the pod and container fields of an isolation tier
// (ADR 0001, design §6.2). Unknown tiers get userns, the default. In the
// userns tier the container also gets /dev/fuse and /dev/net/tun from our
// device plugin (cmd/devices). Call it after setting c.Resources.
func ApplyTier(spec *corev1.PodSpec, c *corev1.Container, tier string) {
	// The mounted identity Secret is mode 0440, group = fsGroup: the agent
	// user (uid and gid roomcfg.AgentUID) can read its own credentials.
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	spec.SecurityContext.FSGroup = ptr.To(int64(1000))
	switch v1.IsolationTier(tier) {
	case v1.TierPrivileged:
		c.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	case v1.TierSysbox:
		spec.RuntimeClassName = ptr.To("sysbox-runc")
	default:
		spec.HostUsers = ptr.To(false)
		c.SecurityContext = &corev1.SecurityContext{
			ProcMount:                ptr.To(corev1.UnmaskedProcMount),
			AllowPrivilegeEscalation: ptr.To(true),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile:          &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
			Capabilities:             &corev1.Capabilities{Add: preflight.RoomCapabilities},
		}
		one := resource.MustParse("1")
		for _, rn := range []corev1.ResourceName{devices.ResourceFuse, devices.ResourceTun} {
			if c.Resources.Limits == nil {
				c.Resources.Limits = corev1.ResourceList{}
			}
			if c.Resources.Requests == nil {
				c.Resources.Requests = corev1.ResourceList{}
			}
			c.Resources.Limits[rn], c.Resources.Requests[rn] = one, one
		}
	}
}

// Overcommitted divides CPU and memory requests by n (operator flag
// --dev-overcommit, for dev and CI clusters smaller than the plan); n <= 1
// leaves them. Limits are never divided, so a room may still use what it was
// planned to.
func Overcommitted(req corev1.ResourceList, n int64) corev1.ResourceList {
	if n <= 1 {
		return req
	}
	out := req.DeepCopy()
	if q, ok := req[corev1.ResourceCPU]; ok {
		out[corev1.ResourceCPU] = *resource.NewMilliQuantity(max(q.MilliValue()/n, 10), resource.DecimalSI)
	}
	if q, ok := req[corev1.ResourceMemory]; ok {
		out[corev1.ResourceMemory] = *resource.NewQuantity(max(q.Value()/n, 64<<20), resource.BinarySI)
	}
	return out
}

// podExtras are what the pod template takes from the layered settings and
// the workspace (§6.6).
type podExtras struct {
	// Overcommit divides the room's requests (--dev-overcommit).
	Overcommit int64
	// Env is the merged environment; nil means the room's own spec.env.
	Env map[string]string
	// Shared are the workspace's shared volumes the room mounts.
	Shared []sharedMount
	// PersonaHash is a hash of the persona ConfigMap's data.
	PersonaHash string
}

// desiredStatefulSet is the room's StatefulSet. Its pod template depends only
// on the Room, its role and workspace settings, the image and the tier, never
// on other rooms.
func desiredStatefulSet(r *v1.Room, ws, image, tier string, x podExtras) *appsv1.StatefulSet {
	name := r.Name
	cpu, mem := defaultCPU, defaultMemory
	if r.Spec.Resources.CPU != nil {
		cpu = *r.Spec.Resources.CPU
	}
	if r.Spec.Resources.Memory != nil {
		mem = *r.Spec.Resources.Memory
	}
	home, engine := defaultHome, defaultEngine
	if r.Spec.Volumes.Home != nil {
		home = *r.Spec.Volumes.Home
	}
	if r.Spec.Volumes.Engine != nil {
		engine = *r.Spec.Volumes.Engine
	}

	env := []corev1.EnvVar{
		{Name: "SWARM_ROOM", Value: name},
		{Name: "SWARM_WORKSPACE", Value: ws},
		{Name: "ANTHROPIC_BASE_URL", Value: GatewayURL},
	}
	vars := x.Env
	if vars == nil {
		vars = r.Spec.Env
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, corev1.EnvVar{Name: k, Value: vars[k]})
	}

	mounts := []corev1.VolumeMount{
		{Name: "home", MountPath: "/home/" + name},
		{Name: "engine", MountPath: "/var/lib/containers"},
		{Name: "identity", MountPath: "/run/swarm/identity", ReadOnly: true},
		{Name: "persona", MountPath: "/etc/swarm/persona", ReadOnly: true},
		{Name: "ca", MountPath: "/etc/swarm/ca", ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "identity", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: IdentitySecretName(name), DefaultMode: ptr.To[int32](0o440)}}},
		{Name: "persona", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: PersonaConfigMapName(name)}, DefaultMode: ptr.To[int32](0o444)}}},
		{Name: "ca", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o444),
			Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: CABundleConfigMap}, Optional: ptr.To(true)}}},
		}}},
	}
	for _, m := range x.Shared {
		vn := "shared-" + m.Name
		volumes = append(volumes, corev1.Volume{Name: vn, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: SharedVolumeClaim(m.Name), ReadOnly: m.ReadOnly}}})
		mounts = append(mounts, corev1.VolumeMount{Name: vn, MountPath: "/srv/shared/" + m.Name, ReadOnly: m.ReadOnly})
	}
	c := corev1.Container{
		Name:    "room",
		Image:   image,
		Command: []string{"tini", "--", "supervisor"},
		Env:     env,
		Resources: corev1.ResourceRequirements{
			Requests: Overcommitted(corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem}, x.Overcommit),
			Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem},
		},
		VolumeMounts: mounts,
	}
	spec := corev1.PodSpec{
		Hostname:                      name,
		ServiceAccountName:            Name(name),
		AutomountServiceAccountToken:  ptr.To(false),
		EnableServiceLinks:            ptr.To(false),
		TerminationGracePeriodSeconds: ptr.To[int64](120),
		Volumes:                       volumes,
	}
	ApplyTier(&spec, &c, tier)
	spec.Containers = []corev1.Container{c}

	replicas := int32(1)
	if r.Spec.Paused {
		replicas = 0
	}
	pvc := func(n string, size resource.Quantity) corev1.PersistentVolumeClaim {
		return corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: n, Labels: objectLabels(r, ws)},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				StorageClassName: r.Spec.Volumes.StorageClassName,
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
			},
		}
	}
	tmpl := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: objectLabels(r, ws)},
		Spec:       spec,
	}
	if x.PersonaHash != "" {
		tmpl.Annotations = map[string]string{AnnotationPersonaHash: x.PersonaHash}
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: Name(name), Namespace: r.Namespace, Labels: objectLabels(r, ws)},
		Spec: appsv1.StatefulSetSpec{
			Replicas:             &replicas,
			ServiceName:          Name(name),
			Selector:             &metav1.LabelSelector{MatchLabels: SelectorLabels(name)},
			Template:             tmpl,
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{pvc("home", home), pvc("engine", engine)},
			UpdateStrategy:       appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType},
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
	sts.Annotations = map[string]string{AnnotationTemplateHash: TemplateHash(&tmpl)}
	return sts
}

// TemplateHash is a stable hash of a pod template as we render it.
func TemplateHash(t *corev1.PodTemplateSpec) string {
	b, _ := json.Marshal(t)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

func desiredService(r *v1.Room, ws string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: Name(r.Name), Namespace: r.Namespace, Labels: objectLabels(r, ws)},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  SelectorLabels(r.Name),
			Ports:     []corev1.ServicePort{{Name: "vnc", Port: vncPort, TargetPort: intstr.FromInt32(vncPort), Protocol: corev1.ProtocolTCP}},
		},
	}
}

func desiredServiceAccount(r *v1.Room, ws string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: Name(r.Name), Namespace: r.Namespace, Labels: objectLabels(r, ws)},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// Egress describes where a room or runner may connect.
type Egress struct {
	// Internet allows 0.0.0.0/0 and ::/0 minus Denied (network mode full).
	Internet bool
	// Proxy allows the egress proxy (network mode limited).
	Proxy bool
	// Quarantined leaves only DNS and the gateway, whatever else is set.
	Quarantined bool
	// Denied are cluster CIDRs and the admin's extra ranges.
	Denied []string
	// Allowed are reachable even inside denied ranges.
	Allowed []string
	// DNS is where the cluster's DNS pods are; the zero value is kube-dns.
	DNS clusterdns.Target
}

// AlwaysDenied are kept from rooms on top of the cluster's own ranges (§11.2).
var AlwaysDenied = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "fc00::/7", "fe80::/10"}

// PlatformPeer is a platform service a pod may reach.
type PlatformPeer struct {
	App   string
	Ports []int32
}

// RoomPlatformPeers are the platform services rooms reach (§11.2), by their
// pod's app.kubernetes.io/name and container ports.
var RoomPlatformPeers = []PlatformPeer{
	{App: "messaging", Ports: []int32{8443}},
	{App: "forgejo", Ports: []int32{3000, 22, 2222}},
	{App: "gateway", Ports: []int32{4000}},
	{App: "registry-cache", Ports: []int32{5000}},
}

// EgressProxyPeer is the egress proxy, for rooms in limited mode.
var EgressProxyPeer = PlatformPeer{App: "egress", Ports: []int32{3128}}

// GatewayPeer is the model gateway (the warden in front of LiteLLM).
var GatewayPeer = PlatformPeer{App: "gateway", Ports: []int32{4000}}

// EgressRules builds egress rules: DNS, the given platform services, and the
// internet per e.
func EgressRules(peers []PlatformPeer, e Egress) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	port := func(p corev1.Protocol, n int32) networkingv1.NetworkPolicyPort {
		return networkingv1.NetworkPolicyPort{Protocol: ptr.To(p), Port: ptr.To(intstr.FromInt32(n))}
	}
	nsSel := func(ns string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}}
	}
	rules := []networkingv1.NetworkPolicyEgressRule{e.DNS.EgressRule()}
	for _, p := range peers {
		r := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
			NamespaceSelector: nsSel(v1.PlatformNamespace),
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelAppName: p.App}},
		}}}
		for _, n := range p.Ports {
			r.Ports = append(r.Ports, port(tcp, n))
		}
		rules = append(rules, r)
	}
	if e.Internet {
		var v4, v6 []string
		for _, c := range append(append([]string{}, AlwaysDenied...), e.Denied...) {
			_, n, err := net.ParseCIDR(strings.TrimSpace(c))
			if err != nil {
				continue
			}
			if n.IP.To4() != nil {
				v4 = append(v4, n.String())
			} else {
				v6 = append(v6, n.String())
			}
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{
			{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: dedup(v4)}},
			{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: dedup(v6)}},
		}})
	}
	if len(e.Allowed) > 0 {
		var to []networkingv1.NetworkPolicyPeer
		for _, c := range dedup(e.Allowed) {
			to = append(to, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: c}})
		}
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: to})
	}
	return rules
}

func dedup(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range s {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

// roomPeers are the platform services a room reaches with egress e.
func roomPeers(e Egress) []PlatformPeer {
	switch {
	case e.Quarantined:
		return []PlatformPeer{GatewayPeer}
	case e.Proxy:
		return append(slices.Clone(RoomPlatformPeers), EgressProxyPeer)
	}
	return RoomPlatformPeers
}

// desiredNetworkPolicy is the room's NetworkPolicy for its network mode, or
// its quarantine (only DNS and the gateway).
func desiredNetworkPolicy(r *v1.Room, ws string, e Egress) *networkingv1.NetworkPolicy {
	if e.Quarantined {
		e = Egress{Quarantined: true, DNS: e.DNS}
	}
	tcp := corev1.ProtocolTCP
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: Name(r.Name), Namespace: r.Namespace, Labels: objectLabels(r, ws)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: SelectorLabels(r.Name)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": v1.SystemNamespace}},
					PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{LabelAppName: "control-center"}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptr.To(intstr.FromInt32(vncPort))}},
			}},
			Egress: EgressRules(roomPeers(e), e),
		},
	}
}
