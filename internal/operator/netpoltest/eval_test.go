package netpoltest

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// endpoint is a pod, or (namespace "") an address outside the cluster's pods.
type endpoint struct {
	name, namespace string
	labels          map[string]string
	ip              netip.Addr
	// ports the pod listens on, by protocol and number; nil for addresses.
	ports map[port]bool
}

type port struct {
	proto corev1.Protocol
	num   int32
}

func (p port) String() string { return fmt.Sprintf("%d/%s", p.num, p.proto) }

// cluster is the policies and the namespaces' labels. It covers what the
// operator's policies use: pod and namespace selectors, ipBlocks with
// exceptions, numbered ports with ranges. Named ports are an error.
type cluster struct {
	namespaces map[string]map[string]string
	policies   []networkingv1.NetworkPolicy
}

// allowed reports whether src may open a connection to dst on p: egress
// from src and ingress to dst must both allow it.
func (c *cluster) allowed(src, dst endpoint, p port) (bool, error) {
	out, err := c.direction(src, dst, p, networkingv1.PolicyTypeEgress)
	if err != nil || !out {
		return false, err
	}
	return c.direction(dst, src, p, networkingv1.PolicyTypeIngress)
}

// direction evaluates the policies selecting self for one direction; peer
// is the other end. With no policy selecting self, everything is allowed.
func (c *cluster) direction(self, peer endpoint, p port, typ networkingv1.PolicyType) (bool, error) {
	if self.namespace == "" {
		return true, nil
	}
	selected := false
	for _, np := range c.policies {
		if np.Namespace != self.namespace || !hasType(np.Spec, typ) {
			continue
		}
		if ok, err := matches(&np.Spec.PodSelector, self.labels); err != nil {
			return false, err
		} else if !ok {
			continue
		}
		selected = true
		type rule struct {
			peers []networkingv1.NetworkPolicyPeer
			ports []networkingv1.NetworkPolicyPort
		}
		var rules []rule
		if typ == networkingv1.PolicyTypeEgress {
			for _, r := range np.Spec.Egress {
				rules = append(rules, rule{r.To, r.Ports})
			}
		} else {
			for _, r := range np.Spec.Ingress {
				rules = append(rules, rule{r.From, r.Ports})
			}
		}
		for _, r := range rules {
			pok, err := portsMatch(r.ports, p)
			if err != nil {
				return false, err
			}
			if !pok {
				continue
			}
			if len(r.peers) == 0 {
				return true, nil
			}
			for _, pr := range r.peers {
				if ok, err := c.peerMatches(pr, np.Namespace, peer); err != nil || ok {
					return ok, err
				}
			}
		}
	}
	return !selected, nil
}

// hasType applies the API's default: Ingress always, Egress when there are
// egress rules.
func hasType(s networkingv1.NetworkPolicySpec, typ networkingv1.PolicyType) bool {
	if len(s.PolicyTypes) > 0 {
		return slices.Contains(s.PolicyTypes, typ)
	}
	return typ == networkingv1.PolicyTypeIngress || len(s.Egress) > 0
}

func (c *cluster) peerMatches(pr networkingv1.NetworkPolicyPeer, policyNS string, e endpoint) (bool, error) {
	if pr.IPBlock != nil {
		cidr, err := netip.ParsePrefix(pr.IPBlock.CIDR)
		if err != nil || !e.ip.IsValid() || !cidr.Contains(e.ip) {
			return false, err
		}
		for _, ex := range pr.IPBlock.Except {
			x, err := netip.ParsePrefix(ex)
			if err != nil {
				return false, err
			}
			if x.Contains(e.ip) {
				return false, nil
			}
		}
		return true, nil
	}
	if e.namespace == "" {
		return false, nil
	}
	if pr.NamespaceSelector != nil {
		nsLabels, ok := c.namespaces[e.namespace]
		if !ok {
			return false, fmt.Errorf("no labels for namespace %s", e.namespace)
		}
		if ok, err := matches(pr.NamespaceSelector, nsLabels); err != nil || !ok {
			return false, err
		}
	} else if e.namespace != policyNS {
		return false, nil
	}
	if pr.PodSelector != nil {
		return matches(pr.PodSelector, e.labels)
	}
	return true, nil
}

func matches(sel *metav1.LabelSelector, l map[string]string) (bool, error) {
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return false, err
	}
	return s.Matches(labels.Set(l)), nil
}

func portsMatch(ports []networkingv1.NetworkPolicyPort, p port) (bool, error) {
	if len(ports) == 0 {
		return true, nil
	}
	for _, pp := range ports {
		proto := corev1.ProtocolTCP
		if pp.Protocol != nil {
			proto = *pp.Protocol
		}
		if proto != p.proto {
			continue
		}
		if pp.Port == nil {
			return true, nil
		}
		if pp.Port.Type != intstr.Int {
			return false, fmt.Errorf("named port %s: not supported", pp.Port.StrVal)
		}
		end := pp.Port.IntVal
		if pp.EndPort != nil {
			end = *pp.EndPort
		}
		if p.num >= pp.Port.IntVal && p.num <= end {
			return true, nil
		}
	}
	return false, nil
}

func TestEvaluator(t *testing.T) {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	p80 := intstr.FromInt32(80)
	c := &cluster{
		namespaces: map[string]map[string]string{"a": {"team": "a"}, "b": {"team": "b"}},
		policies: []networkingv1.NetworkPolicy{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "web"}, Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &p80}},
				}},
			}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "egress"}, Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}}}}},
					{To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &p80}}},
				},
			}},
		},
	}
	client := endpoint{name: "client", namespace: "a", labels: map[string]string{"role": "client"}, ip: netip.MustParseAddr("10.0.0.1")}
	other := endpoint{name: "other", namespace: "a", labels: map[string]string{"role": "other"}, ip: netip.MustParseAddr("10.0.0.2")}
	web := endpoint{name: "web", namespace: "b", labels: map[string]string{"app": "web"}, ip: netip.MustParseAddr("10.0.1.1")}
	db := endpoint{name: "db", namespace: "b", labels: map[string]string{"app": "db"}, ip: netip.MustParseAddr("10.0.1.2")}
	internet := endpoint{name: "internet", ip: netip.MustParseAddr("1.1.1.1")}
	for _, tc := range []struct {
		src, dst endpoint
		p        port
		want     bool
	}{
		{client, web, port{tcp, 80}, true},
		{client, web, port{tcp, 81}, false},  // port
		{client, web, port{udp, 80}, false},  // protocol
		{other, web, port{tcp, 80}, false},   // pod selector
		{web, web, port{tcp, 80}, false},     // namespace selector
		{client, db, port{tcp, 80}, true},    // db has no policy
		{client, db, port{tcp, 5432}, false}, // a's egress: cluster only on 80; 10/8 excepted
		{client, internet, port{tcp, 443}, true},
		{web, internet, port{udp, 53}, true}, // b has no egress policy
		{internet, web, port{tcp, 80}, false},
	} {
		got, err := c.allowed(tc.src, tc.dst, tc.p)
		if err != nil || got != tc.want {
			t.Errorf("%s -> %s %s: %v %v, want %v", tc.src.name, tc.dst.name, tc.p, got, err, tc.want)
		}
	}
}
