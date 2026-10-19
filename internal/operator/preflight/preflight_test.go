package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/usr-lex/swarmageddon/api/v1alpha1"
	"github.com/usr-lex/swarmageddon/internal/operator/images"
)

func TestEvaluateVersion(t *testing.T) {
	cases := map[int]v1alpha1.CheckResult{32: v1alpha1.CheckFail, 33: v1alpha1.CheckWarn, 35: v1alpha1.CheckWarn, 36: v1alpha1.CheckPass, 37: v1alpha1.CheckPass}
	for minor, want := range cases {
		if got := EvaluateVersion(1, minor, "v1.x").Result; got != want {
			t.Errorf("1.%d: got %s want %s", minor, got, want)
		}
	}
}

func chk(r v1alpha1.CheckResult) v1alpha1.PreflightCheck { return v1alpha1.PreflightCheck{Result: r} }

func TestChooseTier(t *testing.T) {
	pass, warn, fail := chk(v1alpha1.CheckPass), chk(v1alpha1.CheckWarn), chk(v1alpha1.CheckFail)
	auto := &v1alpha1.PlatformSpec{}
	accepted := &v1alpha1.PlatformSpec{PrivilegedAccepted: true}
	cases := []struct {
		name         string
		spec         *v1alpha1.PlatformSpec
		minor        int
		idmap, pod   v1alpha1.PreflightCheck
		sysbox, priv bool
		tier         v1alpha1.IsolationTier
		result       v1alpha1.CheckResult
	}{
		{"podman works", auto, 36, pass, pass, false, true, v1alpha1.TierUserns, v1alpha1.CheckPass},
		{"room image missing, idmap ok", auto, 36, pass, warn, false, true, v1alpha1.TierUserns, v1alpha1.CheckWarn},
		{"podman fails, sysbox", auto, 36, pass, fail, true, true, v1alpha1.TierSysbox, v1alpha1.CheckPass},
		{"nothing, privileged not accepted", auto, 32, fail, fail, false, true, v1alpha1.TierPrivileged, v1alpha1.CheckFail},
		{"nothing, privileged accepted", accepted, 32, fail, fail, false, true, v1alpha1.TierPrivileged, v1alpha1.CheckWarn},
		{"nothing at all", accepted, 32, fail, fail, false, false, "", v1alpha1.CheckFail},
		{"pending", auto, 36, chk(v1alpha1.CheckPending), pass, false, true, "", v1alpha1.CheckPending},
		{"forced sysbox missing", &v1alpha1.PlatformSpec{IsolationTier: v1alpha1.TierSysbox}, 36, pass, pass, false, true, v1alpha1.TierSysbox, v1alpha1.CheckFail},
		{"forced userns", &v1alpha1.PlatformSpec{IsolationTier: v1alpha1.TierUserns}, 36, fail, fail, false, true, v1alpha1.TierUserns, v1alpha1.CheckPass},
	}
	for _, c := range cases {
		tier, got := ChooseTier(c.spec, c.minor, c.idmap, c.pod, c.sysbox, c.priv)
		if tier != c.tier || got.Result != c.result {
			t.Errorf("%s: tier %q result %s (%s); want %q %s", c.name, tier, got.Result, got.Message, c.tier, c.result)
		}
	}
}

func node(name, arch, cpu, mem string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelArchStable: arch}},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.42.0.0/24"}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)},
			Addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.10"}},
			NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.36.5+k3s1"},
		},
	}
}

func TestCapacity(t *testing.T) {
	n := node("a", "amd64", "8", "32Gi")
	busy := corev1.Pod{Spec: corev1.PodSpec{NodeName: "a", Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}}}}}}
	est, c := capacity([]corev1.Node{n}, []corev1.Pod{busy})
	// 6 CPU / 2 = 3; 28Gi / 6Gi = 4 -> 3 rooms.
	if est != 3 || c.Result != v1alpha1.CheckPass {
		t.Fatalf("estimate %d (%s)", est, c.Message)
	}
	small := node("b", "arm64", "1", "2Gi")
	if est, c := capacity([]corev1.Node{small}, nil); est != 0 || c.Result != v1alpha1.CheckWarn {
		t.Fatalf("small node: %d %s", est, c.Result)
	}
}

func TestClusterCIDRs(t *testing.T) {
	cl := &cluster{nodes: []corev1.Node{node("a", "amd64", "4", "8Gi")}, serviceCIDRs: []string{"10.43.0.0/16"}, apiEndpoints: []string{"192.168.1.10"}}
	cidrs, c := clusterCIDRs(&v1alpha1.PlatformSpec{}, cl)
	if c.Result != v1alpha1.CheckPass || strings.Join(cidrs, ",") != "10.42.0.0/24,10.43.0.0/16,192.168.1.10/32" {
		t.Fatalf("%v %s %s", cidrs, c.Result, c.Message)
	}
	cl.serviceCIDRs = nil
	cl.kubernetesIP = "10.43.0.1"
	_, c = clusterCIDRs(&v1alpha1.PlatformSpec{}, cl)
	if c.Result != v1alpha1.CheckWarn {
		t.Fatalf("missing service CIDR should warn: %s", c.Message)
	}
	cidrs, c = clusterCIDRs(&v1alpha1.PlatformSpec{Egress: v1alpha1.EgressSpec{ClusterCIDRs: []string{"10.0.0.0/8"}}}, cl)
	if c.Result != v1alpha1.CheckPass || len(cidrs) != 1 {
		t.Fatal("spec override ignored")
	}
}

func TestSizeEnforcement(t *testing.T) {
	if sizeEnforcement("rancher.io/local-path").Result != v1alpha1.CheckWarn || sizeEnforcement("topolvm.io").Result != v1alpha1.CheckPass {
		t.Fatal("provisioner classification")
	}
}

func TestClassifyImagePull(t *testing.T) {
	r := &Runner{Now: time.Now}
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Image: "room", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}
	if p := r.classify(pod); p.state != stateImageUnavailable {
		t.Fatalf("state %v", p.state)
	}
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	if p := r.classify(old); p.state != stateTimedOut {
		t.Fatalf("state %v", p.state)
	}
}

// TestRunWithFakeCluster drives Run through pending probes to completion by
// playing kubelet: it marks probe pods as finished.
func TestRunWithFakeCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	n := node("a", "amd64", "16", "64Gi")
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-path", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "rancher.io/local-path"}
	ic := &networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik", Annotations: map[string]string{networkingv1.AnnotationIsDefaultIngressClass: "true"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&n, sc, ic).WithStatusSubresource(&corev1.Pod{}).Build()
	r := &Runner{Client: c, Images: images.Defaults("v1"), ServerVersion: func() (*version.Info, error) {
		return &version.Info{Major: "1", Minor: "36", GitVersion: "v1.36.5+k3s1"}, nil
	}}
	ctx := context.Background()
	spec := &v1alpha1.PlatformSpec{}

	res, err := r.Run(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("done before any probe ran")
	}
	// Play kubelet a few rounds: servers run, everything else succeeds.
	for range 4 {
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.InNamespace(Namespace)); err != nil {
			t.Fatal(err)
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase != "" {
				continue
			}
			switch p.Name {
			case podNPOpen, podNPClosed:
				p.Status.Phase, p.Status.PodIP = corev1.PodRunning, "10.42.0."+map[string]string{podNPOpen: "5", podNPClosed: "6"}[p.Name]
			default:
				p.Status.Phase = corev1.PodSucceeded
			}
			if err := c.Status().Update(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		if res, err = r.Run(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	if !res.Done {
		for _, ch := range res.Status.Checks {
			t.Logf("%s: %s %s", ch.Name, ch.Result, ch.Message)
		}
		t.Fatal("not done")
	}
	if res.Status.Tier != v1alpha1.TierUserns || res.Status.RoomEstimate != 8 {
		t.Fatalf("tier %q estimate %d", res.Status.Tier, res.Status.RoomEstimate)
	}
	got := map[string]v1alpha1.CheckResult{}
	for _, ch := range res.Status.Checks {
		got[ch.Name] = ch.Result
	}
	for name, want := range map[string]v1alpha1.CheckResult{
		CheckNetworkPolicy: v1alpha1.CheckPass, CheckVolumeSharing: v1alpha1.CheckPass, CheckIdmap: v1alpha1.CheckPass,
		CheckSizeEnforcement: v1alpha1.CheckWarn, CheckIngress: v1alpha1.CheckPass, CheckPodmanUserns: v1alpha1.CheckPass,
	} {
		if got[name] != want {
			t.Errorf("%s = %s, want %s", name, got[name], want)
		}
	}
	if err := r.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	var left corev1.PodList
	_ = c.List(ctx, &left, client.InNamespace(Namespace))
	if len(left.Items) != 0 {
		t.Fatalf("%d pods left after cleanup", len(left.Items))
	}
}

// An admission policy refusing the probe pods is said as such, and the tier
// check's fix points at it rather than at the kernel (OpenShift report 5.4:
// "probe error: pods \"idmap\" is forbidden: unable to validate against any
// security context constraint" became "use a 6.3+ kernel").
func TestAdmissionRefusal(t *testing.T) {
	err := apierrors.NewForbidden(corev1.Resource("pods"), "idmap",
		errors.New("unable to validate against any security context constraint: [provider \"restricted-v2\": Forbidden: not usable by user or serviceaccount]"))
	c := probeErr(CheckIdmap, err)
	if c.Result != v1alpha1.CheckFail || !strings.HasPrefix(c.Message, refusedPrefix) || !strings.Contains(c.Message, "security context constraint") || c.Fix == "" {
		t.Fatalf("idmap check: %+v", c)
	}
	podman := probeErr(CheckPodmanUserns, err)
	_, tier := ChooseTier(&v1alpha1.PlatformSpec{}, 35, c, podman, false, false)
	if tier.Result != v1alpha1.CheckFail || !strings.Contains(tier.Message, "admission") || strings.Contains(tier.Fix, "kernel") {
		t.Fatalf("tier check: %+v", tier)
	}
	// Other errors stay warnings.
	if c := probeErr(CheckIdmap, errors.New("connection refused")); c.Result != v1alpha1.CheckWarn {
		t.Fatalf("plain error: %+v", c)
	}
}

// OVN-Kubernetes leaves nodes' podCIDR empty; OpenShift's Network "cluster"
// has the ranges (OpenShift report 5.7).
func TestOpenShiftNetworks(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "config.openshift.io/v1", "kind": "Network", "metadata": map[string]any{"name": "cluster"},
		"status": map[string]any{
			"clusterNetwork": []any{map[string]any{"cidr": "10.217.0.0/22", "hostPrefix": int64(23)}},
			"serviceNetwork": []any{"10.217.4.0/23"},
		},
	}}
	c := fake.NewClientBuilder().WithObjects(u).Build()
	pods, services := openShiftNetworks(context.Background(), c)
	if len(pods) != 1 || pods[0] != "10.217.0.0/22" || len(services) != 1 || services[0] != "10.217.4.0/23" {
		t.Fatalf("pods %v services %v", pods, services)
	}
	cl := &cluster{nodes: []corev1.Node{node("n1", "arm64", "12", "32Gi")}, podCIDRs: pods, serviceCIDRs: services}
	cl.nodes[0].Spec.PodCIDRs = nil
	if _, chk := clusterCIDRs(&v1alpha1.PlatformSpec{}, cl); chk.Result != v1alpha1.CheckPass {
		t.Fatalf("check %+v", chk)
	}
	if c := ingressCheck(nil, true); !strings.Contains(c.Fix, "create route passthrough") {
		t.Fatalf("ingress fix %q", c.Fix)
	}
}
