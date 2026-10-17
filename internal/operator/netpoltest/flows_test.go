package netpoltest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/forgejo"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/operator/manifests"
	"github.com/usr-lex/swarmageddon/internal/operator/room"
	"github.com/usr-lex/swarmageddon/internal/operator/room/roomtest"
	"github.com/usr-lex/swarmageddon/internal/operator/runnerpool"
	"github.com/usr-lex/swarmageddon/internal/operator/workspace"
)

const (
	sys  = v1alpha1.SystemNamespace
	plat = v1alpha1.PlatformNamespace
)

// Addresses outside the pods. The API server is reached at a node's
// address after the kubernetes Service's DNAT, which is what policies see.
var addresses = map[string]string{
	"internet":         "1.1.1.1",
	"cloud metadata":   "169.254.169.254",
	"lan host":         "192.168.1.10",
	"kube API":         "192.168.5.10",
	"public kube API":  "203.0.113.10",
	"kubernetes svc":   "10.43.0.1",
	"other pod subnet": "10.42.200.1",
}

// clusterCIDRs is what preflight finds: pod and service ranges and the
// nodes' addresses (rooms and runners may not reach them).
var clusterCIDRs = []string{"10.42.0.0/16", "10.43.0.0/16", "192.168.5.10/32", "203.0.113.10/32"}

type model struct {
	cluster
	endpoints map[string]endpoint
	services  []corev1.Service
	nextIP    int
}

func repoPath(rel string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", rel)
}

func decodeFile(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := manifests.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func fromUnstructured[T any](t *testing.T, u *unstructured.Unstructured) *T {
	t.Helper()
	var out T
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func (m *model) namespace(name string, l map[string]string) {
	if m.namespaces[name] == nil {
		m.namespaces[name] = map[string]string{"kubernetes.io/metadata.name": name}
	}
	for k, v := range l {
		m.namespaces[name][k] = v
	}
}

// pod adds a pod from a pod template.
func (m *model) pod(name, ns string, tmpl corev1.PodTemplateSpec) {
	m.namespace(ns, nil)
	m.nextIP++
	e := endpoint{name: name, namespace: ns, labels: tmpl.Labels, ports: map[port]bool{},
		ip: netip.MustParseAddr(fmt.Sprintf("10.42.%d.%d", m.nextIP/250, m.nextIP%250+1))}
	for _, c := range tmpl.Spec.Containers {
		for _, p := range c.Ports {
			e.ports[port{protoOf(p.Protocol), p.ContainerPort}] = true
		}
	}
	m.endpoints[name] = e
}

func protoOf(p corev1.Protocol) corev1.Protocol {
	if p == "" {
		return corev1.ProtocolTCP
	}
	return p
}

// objects adds pods, Services, policies and namespaces from manifests.
func (m *model) objects(t *testing.T, objs []*unstructured.Unstructured) {
	t.Helper()
	for _, o := range objs {
		switch o.GetKind() {
		case "Namespace":
			m.namespace(o.GetName(), o.GetLabels())
		case "Deployment":
			m.pod(o.GetName(), o.GetNamespace(), fromUnstructured[appsv1.Deployment](t, o).Spec.Template)
		case "StatefulSet":
			m.pod(o.GetName(), o.GetNamespace(), fromUnstructured[appsv1.StatefulSet](t, o).Spec.Template)
		case "DaemonSet":
			m.pod(o.GetName(), o.GetNamespace(), fromUnstructured[appsv1.DaemonSet](t, o).Spec.Template)
		case "Job":
			m.pod(o.GetName(), o.GetNamespace(), fromUnstructured[batchv1.Job](t, o).Spec.Template)
		case "Service":
			m.services = append(m.services, *fromUnstructured[corev1.Service](t, o))
		case "NetworkPolicy":
			m.policies = append(m.policies, *fromUnstructured[networkingv1.NetworkPolicy](t, o))
		}
	}
}

// servicePorts adds the Services' target ports to the pods they select,
// so a policy is checked against the port traffic actually arrives on.
func (m *model) servicePorts() {
	for _, s := range m.services {
		if len(s.Spec.Selector) == 0 {
			continue
		}
		for name, e := range m.endpoints {
			if e.namespace != s.Namespace {
				continue
			}
			if ok, _ := matches(&metav1.LabelSelector{MatchLabels: s.Spec.Selector}, e.labels); !ok {
				continue
			}
			for _, sp := range s.Spec.Ports {
				tp := sp.TargetPort
				if tp.Type == intstr.String {
					// Named: a container port of that name, which the
					// pod's ports already have.
					continue
				}
				n := tp.IntVal
				if n == 0 {
					n = sp.Port
				}
				m.endpoints[name].ports[port{protoOf(sp.Protocol), n}] = true
			}
		}
	}
}

// build makes the cluster: the platform templates as rendered with test
// values (the goldens TestGolden keeps current), the chart, the workspace,
// room and runner pool controllers' objects for two workspaces, and
// kube-system's DNS and Traefik.
func build(t *testing.T) *model {
	t.Helper()
	m := &model{cluster: cluster{namespaces: map[string]map[string]string{}}, endpoints: map[string]endpoint{}}
	goldens, err := filepath.Glob(repoPath("internal/operator/manifests/testdata/*.golden.yaml"))
	if err != nil || len(goldens) == 0 {
		t.Fatalf("goldens: %v", err)
	}
	for _, g := range goldens {
		m.objects(t, decodeFile(t, g))
	}
	m.objects(t, decodeFile(t, repoPath("deploy/install.yaml")))

	// kube-system as k3s has it: CoreDNS and the packaged Traefik chart.
	m.pod("kube-dns", "kube-system", corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"k8s-app": "kube-dns"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Ports: []corev1.ContainerPort{
			{ContainerPort: 53, Protocol: corev1.ProtocolUDP}, {ContainerPort: 53, Protocol: corev1.ProtocolTCP}}}}},
	})
	m.pod("traefik", "kube-system", corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/name": "traefik", "app.kubernetes.io/instance": "traefik-kube-system"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Ports: []corev1.ContainerPort{{ContainerPort: 8000}, {ContainerPort: 8443}}}}},
	})

	controllers(t, m)
	m.servicePorts()
	for name, a := range addresses {
		m.endpoints[name] = endpoint{name: name, ip: netip.MustParseAddr(a)}
	}
	return m
}

// Rooms by network mode, in workspace acme, and one in workspace other.
var rooms = map[string]struct {
	ws          string
	mode        v1alpha1.NetworkMode
	quarantined bool
}{
	"nora":   {"acme", v1alpha1.NetworkNone, false},
	"lima":   {"acme", v1alpha1.NetworkLimited, false},
	"fuller": {"acme", v1alpha1.NetworkFull, false},
	"quinn":  {"acme", v1alpha1.NetworkFull, true},
	"otto":   {"other", v1alpha1.NetworkFull, false},
}

// controllers runs the workspace, room and runner pool reconcilers on a
// fake API server and adds what they made.
func controllers(t *testing.T, m *model) {
	t.Helper()
	ctx := context.Background()
	scheme := roomtest.Scheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{Apply: replace}).
		WithStatusSubresource(&v1alpha1.Workspace{}, &v1alpha1.Room{}, &v1alpha1.RunnerPool{}, &v1alpha1.Platform{}).Build()
	must(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: plat}}))
	p := &v1alpha1.Platform{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.PlatformName}}
	must(t, c.Create(ctx, p))
	p.Status.Preflight = &v1alpha1.PreflightStatus{ClusterCIDRs: clusterCIDRs}
	must(t, c.Status().Update(ctx, p))

	pki := roomtest.NewPKI()
	forge := roomtest.NewForge(t, "implementers", "reviewers", "mergers")
	gw := roomtest.NewGateway(t)
	d := deps.Deps{PKI: pki, Images: &roomtest.Images{RoomImage: "room:1", RunnerImage: "runner:1"}, Forge: forge.Client,
		Gateway: gw.Client, PlatformTier: func(context.Context) (string, error) { return "userns", nil }}

	wr := &workspace.Reconciler{Client: c, Scheme: scheme, Deps: deps.Deps{PKI: pki}}
	for _, ws := range []string{"acme", "other"} {
		must(t, c.Create(ctx, &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: ws}, Spec: v1alpha1.WorkspaceSpec{
			Forge: v1alpha1.ForgeSpec{Org: ws}}}))
		if _, err := wr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: ws}}); err != nil {
			t.Fatalf("workspace %s: %v", ws, err)
		}
	}
	rr := &room.Reconciler{Client: c, Deps: d, Now: func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }}
	for name, r := range rooms {
		ns := v1alpha1.WorkspaceNamespace(r.ws)
		must(t, c.Create(ctx, &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: v1alpha1.RoomSpec{
			Role: "implementer", CLI: v1alpha1.CLIClaude, Quarantined: r.quarantined, Network: &v1alpha1.NetworkAccess{Mode: r.mode}}}))
		for range 3 {
			if _, err := rr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
				t.Fatalf("room %s: %v", name, err)
			}
		}
	}
	must(t, c.Create(ctx, &v1alpha1.RunnerPool{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "ws-acme"},
		Spec: v1alpha1.RunnerPoolSpec{Labels: []string{"swarm"}, MaxConcurrent: 1}}))
	forge.SetJobs([]forgejo.RunJob{{ID: 1, Status: "waiting", RunsOn: []string{"swarm"}}})
	pr := &runnerpool.Reconciler{Client: c, Deps: d}
	if _, err := pr.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ws-acme", Name: "ci"}}); err != nil {
		t.Fatalf("runner pool: %v", err)
	}

	var nss corev1.NamespaceList
	must(t, c.List(ctx, &nss))
	for _, n := range nss.Items {
		m.namespace(n.Name, n.Labels)
	}
	var sts appsv1.StatefulSetList
	must(t, c.List(ctx, &sts))
	for _, s := range sts.Items {
		m.pod(s.Name, s.Namespace, s.Spec.Template)
	}
	var pods corev1.PodList
	must(t, c.List(ctx, &pods))
	for _, p := range pods.Items {
		m.pod(p.Name, p.Namespace, corev1.PodTemplateSpec{ObjectMeta: p.ObjectMeta, Spec: p.Spec})
	}
	var svcs corev1.ServiceList
	must(t, c.List(ctx, &svcs))
	m.services = append(m.services, svcs.Items...)
	var nps networkingv1.NetworkPolicyList
	must(t, c.List(ctx, &nps))
	m.policies = append(m.policies, nps.Items...)
	for _, want := range []string{"room-nora", "room-otto", "runner-ci-1"} {
		if _, ok := m.endpoints[want]; !ok {
			t.Fatalf("the controllers made no %s", want)
		}
	}
}

// replace stands in for server-side apply, which the fake client can't do
// for every type: create the object, or replace the one there.
func replace(ctx context.Context, c client.WithWatch, obj k8sruntime.ApplyConfiguration, _ ...client.ApplyOption) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(b); err != nil {
		return err
	}
	cur := &unstructured.Unstructured{}
	cur.SetGroupVersionKind(u.GroupVersionKind())
	err = c.Get(ctx, client.ObjectKeyFromObject(u), cur)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, u)
	}
	if err != nil {
		return err
	}
	u.SetResourceVersion(cur.GetResourceVersion())
	return c.Update(ctx, u)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type flow struct {
	from, to string
	port     int32
	udp      bool
	want     bool
}

func (m *model) check(t *testing.T, flows []flow) {
	t.Helper()
	for _, f := range flows {
		src, ok := m.endpoints[f.from]
		dst, ok2 := m.endpoints[f.to]
		if !ok || !ok2 {
			t.Errorf("flow %s -> %s: unknown endpoint", f.from, f.to)
			continue
		}
		p := port{corev1.ProtocolTCP, f.port}
		if f.udp {
			p.proto = corev1.ProtocolUDP
		}
		if dst.ports != nil && !dst.ports[p] {
			t.Errorf("flow %s -> %s %s: %s doesn't listen on %s", f.from, f.to, p, f.to, p)
			continue
		}
		got, err := m.allowed(src, dst, p)
		if err != nil {
			t.Errorf("flow %s -> %s %s: %v", f.from, f.to, p, err)
		} else if got != f.want {
			verb := "refused"
			if got {
				verb = "allowed"
			}
			t.Errorf("%s -> %s %s is %s", f.from, f.to, p, verb)
		}
	}
}

func TestDocumentedFlows(t *testing.T) {
	m := build(t)
	allow := func(from, to string, port int32) flow { return flow{from: from, to: to, port: port, want: true} }
	deny := func(from, to string, port int32) flow { return flow{from: from, to: to, port: port} }
	dns := func(from string) []flow {
		return []flow{{from: from, to: "kube-dns", port: 53, udp: true, want: true}, allow(from, "kube-dns", 53)}
	}
	var flows []flow
	// §11.2: every mode reaches the platform services and DNS.
	for _, r := range []string{"room-nora", "room-lima", "room-fuller"} {
		flows = append(flows, dns(r)...)
		flows = append(flows,
			allow(r, "messaging", 8443), allow(r, "forgejo", 3000), allow(r, "forgejo", 2222),
			allow(r, "warden", 4000), allow(r, "registry-cache", 5000),
			// The control center's VNC tab; nothing else reaches a room.
			allow("control-center", r, 5900),
			deny("room-otto", r, 5900), deny("runner-ci-1", r, 5900), deny("egress", r, 5900),
			// §11.1: not the control center, the API server, LiteLLM
			// past the warden, the database, NATS routes or other workspaces.
			deny(r, "control-center", 8443), deny(r, "kube API", 6443), deny(r, "public kube API", 6443),
			deny(r, "kubernetes svc", 443), deny(r, "litellm", 4000), deny(r, "postgres", 5432),
			deny(r, "messaging", 6222), deny(r, "ntfy", 8080), deny(r, "room-otto", 5900), deny(r, "other pod subnet", 22),
		)
	}
	flows = append(flows,
		deny("room-nora", "room-lima", 5900),
		// none: nothing outside; limited: the egress proxy only.
		deny("room-nora", "internet", 443), deny("room-nora", "egress", 3128),
		allow("room-lima", "egress", 3128), deny("room-lima", "internet", 443),
		// full: the internet but private, cluster, link-local and node ranges.
		allow("room-fuller", "internet", 443), allow("room-fuller", "internet", 80),
		deny("room-fuller", "cloud metadata", 80), deny("room-fuller", "lan host", 22),
		// Quarantine leaves DNS and the gateway.
		allow("room-quinn", "warden", 4000), allow("room-quinn", "kube-dns", 53),
		deny("room-quinn", "messaging", 8443), deny("room-quinn", "forgejo", 3000), deny("room-quinn", "internet", 443),
		// CI runners: Forgejo, the registry cache, the internet.
		allow("runner-ci-1", "forgejo", 3000), allow("runner-ci-1", "forgejo", 2222),
		allow("runner-ci-1", "registry-cache", 5000), allow("runner-ci-1", "internet", 443),
		deny("runner-ci-1", "messaging", 8443), deny("runner-ci-1", "warden", 4000),
		deny("runner-ci-1", "control-center", 8443), deny("runner-ci-1", "kube API", 6443),
		// The control center's clients.
		allow("control-center", "messaging", 8443), allow("control-center", "warden", 4000),
		allow("control-center", "litellm", 4000), allow("control-center", "postgres", 5432),
		allow("control-center", "ntfy", 8080), allow("control-center", "forgejo", 3000),
		// The operator's: Forgejo and LiteLLM admin, messaging.
		allow("swarmageddon-operator", "forgejo", 3000), allow("swarmageddon-operator", "litellm", 4000),
		allow("swarmageddon-operator", "messaging", 8443), allow("swarmageddon-operator", "messaging", 8080),
		// Platform services' own clients.
		allow("warden", "litellm", 4000), allow("warden", "messaging", 8443),
		allow("forgejo", "postgres", 5432), allow("litellm", "postgres", 5432),
		allow("postgres-bootstrap-abc123", "postgres", 5432), allow("messaging", "messaging", 6222),
		deny("registry-cache", "postgres", 5432), deny("registry-cache", "messaging", 8443),
		deny("ntfy", "litellm", 4000), deny("egress", "messaging", 8443),
		// The egress proxy: DNS, the API server (its allowlist), the internet.
		allow("egress", "internet", 443), allow("egress", "kube API", 6443), allow("egress", "kube-dns", 53),
		deny("egress", "lan host", 80),
		// Who reaches the control center: the ingress controller and
		// Forgejo's webhooks only.
		allow("traefik", "control-center", 8443), allow("forgejo", "control-center", 8443),
		deny("messaging", "control-center", 8443), deny("litellm", "control-center", 8443),
		deny("warden", "control-center", 8443), deny("registry-cache", "control-center", 8443),
		deny("internet", "control-center", 8443),
		// The ingress controller publishes Forgejo and ntfy.
		allow("traefik", "forgejo", 3000), allow("traefik", "ntfy", 8080), deny("traefik", "messaging", 8443),
	)
	m.check(t, flows)
}

// TestSentryReachable: the components that report to Sentry (internal/telemetry)
// reach a SaaS DSN's host on 443 with no policy change. The system
// namespace and the devices namespace have no egress policies; in the
// platform namespace only the egress proxy has one, and it already allows
// 443 anywhere. Rooms and runners don't report, and stay denied.
func TestSentryReachable(t *testing.T) {
	m := build(t)
	var flows []flow
	for _, c := range []string{"swarmageddon-operator", "control-center", "messaging", "warden", "egress", "devices"} {
		flows = append(flows, flow{from: c, to: "internet", port: 443, want: true})
	}
	flows = append(flows, flow{from: "room-nora", to: "internet", port: 443}, flow{from: "room-lima", to: "internet", port: 443})
	m.check(t, flows)
}

// TestNodePortControlCenter: without an IngressClass, clients reach the
// control center's node port from any address; rooms and runners still don't.
func TestNodePortControlCenter(t *testing.T) {
	m := build(t)
	objs, err := manifests.Render(manifests.ControlCenterNetworkPolicy, manifests.Values{Namespace: plat, SystemNamespace: sys})
	must(t, err)
	var kept []networkingv1.NetworkPolicy
	for _, p := range m.policies {
		if p.Namespace != sys || p.Name != "control-center" {
			kept = append(kept, p)
		}
	}
	m.policies = append(kept, *fromUnstructured[networkingv1.NetworkPolicy](t, objs[0]))
	m.check(t, []flow{
		{from: "internet", to: "control-center", port: 8443, want: true},
		{from: "forgejo", to: "control-center", port: 8443, want: true},
		{from: "room-fuller", to: "control-center", port: 8443},
		{from: "runner-ci-1", to: "control-center", port: 8443},
	})
}
