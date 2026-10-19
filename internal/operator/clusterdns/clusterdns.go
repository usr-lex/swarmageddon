// Package clusterdns finds the pods that serve the cluster's DNS, so
// NetworkPolicies can allow exactly them: kube-system's kube-dns on port 53
// on k3s and most clusters, openshift-dns's dns-default on port 5353 on
// OpenShift (OpenShift report, issue 5.2). Policies match the pod's port
// after the Service's translation, so the Service port alone isn't enough.
package clusterdns

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Target is where pods send DNS: the DNS pods' namespace and labels, and
// the ports they listen on (53 always included).
type Target struct {
	Namespace string
	Labels    map[string]string
	Ports     []int32
}

// Default is kube-system's CoreDNS, what k3s, kubeadm and most managed
// clusters run.
var Default = Target{Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}, Ports: []int32{53}}

// wellKnown are DNS Services to try when the nameserver's address matches
// no Service.
var wellKnown = []types.NamespacedName{
	{Namespace: "kube-system", Name: "kube-dns"},
	{Namespace: "openshift-dns", Name: "dns-default"},
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=list

// Finder discovers the Target and caches it. A nil Finder returns Default.
type Finder struct {
	Reader client.Reader
	// ResolvConf is read for the nameserver pods use (the operator's own pod
	// gets the same one); empty means /etc/resolv.conf.
	ResolvConf string
	// TTL of the cached Target; 0 means 10 minutes.
	TTL time.Duration

	mu     sync.Mutex
	cached *Target
	at     time.Time
}

// Target returns the cluster's DNS target. When discovery fails it returns
// Default and the error, so callers can still write a policy.
func (f *Finder) Target(ctx context.Context) (Target, error) {
	if f == nil || f.Reader == nil {
		return Default, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ttl := f.TTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	if f.cached != nil && time.Since(f.at) < ttl {
		return *f.cached, nil
	}
	t, err := f.discover(ctx)
	if err != nil {
		return Default, err
	}
	f.cached, f.at = &t, time.Now()
	return t, nil
}

func (f *Finder) discover(ctx context.Context) (Target, error) {
	svc, err := f.service(ctx)
	if err != nil {
		return Target{}, err
	}
	if len(svc.Spec.Selector) == 0 {
		return Target{}, fmt.Errorf("DNS Service %s/%s has no selector", svc.Namespace, svc.Name)
	}
	t := Target{Namespace: svc.Namespace, Labels: svc.Spec.Selector, Ports: []int32{53}}
	var named []string
	for _, p := range svc.Spec.Ports {
		if p.Port != 53 {
			continue
		}
		switch {
		case p.TargetPort.IntVal != 0:
			t.Ports = append(t.Ports, p.TargetPort.IntVal)
		case p.TargetPort.StrVal != "":
			named = append(named, p.TargetPort.StrVal)
		default:
			t.Ports = append(t.Ports, p.Port)
		}
	}
	if len(named) > 0 {
		// A named target port: the EndpointSlices carry the numbers.
		var eps discoveryv1.EndpointSliceList
		if err := f.Reader.List(ctx, &eps, client.InNamespace(svc.Namespace),
			client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name}); err != nil {
			return Target{}, err
		}
		for _, s := range eps.Items {
			for _, p := range s.Ports {
				if p.Name != nil && p.Port != nil && portNamed(svc, *p.Name, named) {
					t.Ports = append(t.Ports, *p.Port)
				}
			}
		}
	}
	slices.Sort(t.Ports)
	t.Ports = slices.Compact(t.Ports)
	return t, nil
}

// portNamed reports whether the EndpointSlice port name (the Service port's
// name) belongs to a DNS port with a named target.
func portNamed(svc *corev1.Service, name string, targets []string) bool {
	for _, p := range svc.Spec.Ports {
		if p.Name == name && p.Port == 53 && slices.Contains(targets, p.TargetPort.StrVal) {
			return true
		}
	}
	return false
}

// service finds the Service behind the pods' nameserver.
func (f *Finder) service(ctx context.Context) (*corev1.Service, error) {
	if ip := f.nameserver(); ip != "" {
		var all corev1.ServiceList
		if err := f.Reader.List(ctx, &all); err != nil {
			return nil, err
		}
		for i := range all.Items {
			s := &all.Items[i]
			if s.Spec.ClusterIP == ip || slices.Contains(s.Spec.ClusterIPs, ip) {
				return s, nil
			}
		}
	}
	for _, key := range wellKnown {
		var s corev1.Service
		if err := f.Reader.Get(ctx, key, &s); err == nil {
			return &s, nil
		} else if client.IgnoreNotFound(err) != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no DNS Service found (looked for the nameserver's ClusterIP and %v)", wellKnown)
}

// nameserver is the first nameserver in resolv.conf, or "".
func (f *Finder) nameserver() string {
	path := f.ResolvConf
	if path == "" {
		path = "/etc/resolv.conf"
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil {
			return fields[1]
		}
	}
	return ""
}

// EgressRule allows DNS to the target's pods, over UDP and TCP on each of
// its ports. The zero Target means Default.
func (t Target) EgressRule() networkingv1.NetworkPolicyEgressRule {
	if t.Namespace == "" {
		t = Default
	}
	r := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": t.Namespace}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: t.Labels},
	}}}
	for _, p := range t.Ports {
		for _, proto := range []corev1.Protocol{corev1.ProtocolUDP, corev1.ProtocolTCP} {
			r.Ports = append(r.Ports, networkingv1.NetworkPolicyPort{Protocol: ptr.To(proto), Port: ptr.To(intstr.FromInt32(p))})
		}
	}
	return r
}
