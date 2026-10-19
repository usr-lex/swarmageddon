package clusterdns

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func resolv(t *testing.T, ns string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(p, []byte("search default.svc.cluster.local\nnameserver "+ns+"\noptions ndots:5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// OpenShift 4.22: Service openshift-dns/dns-default, port 53 to the named
// target port "dns", which the pods serve on 5353.
func TestOpenShift(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-dns", Name: "dns-default"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "172.30.0.10", ClusterIPs: []string{"172.30.0.10"},
			Selector: map[string]string{"dns.operator.openshift.io/daemonset-dns": "default"},
			Ports: []corev1.ServicePort{
				{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromString("dns")},
				{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("dns-tcp")},
				{Name: "metrics", Port: 9154, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromString("metrics")},
			},
		},
	}
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-dns", Name: "dns-default-abc",
			Labels: map[string]string{discoveryv1.LabelServiceName: "dns-default"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{
			{Name: ptr.To("dns"), Port: ptr.To[int32](5353), Protocol: ptr.To(corev1.ProtocolUDP)},
			{Name: ptr.To("dns-tcp"), Port: ptr.To[int32](5353), Protocol: ptr.To(corev1.ProtocolTCP)},
			{Name: ptr.To("metrics"), Port: ptr.To[int32](9154), Protocol: ptr.To(corev1.ProtocolTCP)},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(svc, es).Build()
	f := &Finder{Reader: c, ResolvConf: resolv(t, "172.30.0.10")}
	got, err := f.Target(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Target{Namespace: "openshift-dns", Labels: map[string]string{"dns.operator.openshift.io/daemonset-dns": "default"}, Ports: []int32{53, 5353}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// k3s: kube-system/kube-dns with numeric target ports 53.
func TestK3s(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-dns"},
		Spec: corev1.ServiceSpec{
			ClusterIP: "10.43.0.10", Selector: map[string]string{"k8s-app": "kube-dns"},
			Ports: []corev1.ServicePort{
				{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(53)},
				{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(53)},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(svc).Build()
	// A resolv.conf that names no Service: the well-known name is used.
	f := &Finder{Reader: c, ResolvConf: resolv(t, "192.0.2.1")}
	got, err := f.Target(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, Default) {
		t.Fatalf("got %+v, want the default", got)
	}
}

// Nothing found: Default and an error; a nil Finder: Default.
func TestFallback(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	f := &Finder{Reader: c, ResolvConf: resolv(t, "192.0.2.1")}
	got, err := f.Target(context.Background())
	if err == nil || !reflect.DeepEqual(got, Default) {
		t.Fatalf("got %+v, %v", got, err)
	}
	var nf *Finder
	if got, err := nf.Target(context.Background()); err != nil || !reflect.DeepEqual(got, Default) {
		t.Fatalf("nil finder: %+v, %v", got, err)
	}
}
